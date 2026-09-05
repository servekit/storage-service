// Link + retention lifecycle for the file domain.
//
// The link model (QQ-mail "large attachment" shape):
//   - CreateFileLink mints a random token (stored on the file row) and
//     optionally sets/renews the retention window. The token is deliberately
//     NOT derived from file_id or retain_until, so renewals never invalidate
//     links already embedded in sent emails.
//   - GetFileLinkDownload is the anonymous backend for those links: token IS
//     the credential. It refuses the file the moment retain_until passes
//     (correctness does not depend on GC timing) and otherwise mints a fresh
//     short-TTL presigned URL per request.
//   - ReapExpiredFiles is the two-stage retention GC: expired files are first
//     marked (releasing object ref-count and quota — mirroring the user-delete
//     transaction), then purged from the DB after a configurable delay that
//     doubles as the recovery window for mistaken expiries.
package file

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
)

// linkDownloadPresignTTL is how long each presigned URL minted by
// GetFileLinkDownload lives. Short by design: recipients' links point at the
// stable link surface, and a fresh URL is minted on every click.
const linkDownloadPresignTTL = 5 * time.Minute

// RetentionGCResult reports one retention-GC pass for observability.
type RetentionGCResult struct {
	MarkedExpired int
	Purged        int
}

// CreateFileLink mints (or returns) the anonymous link token for a file and
// optionally sets or renews its retention window.
func (s *Service) CreateFileLink(ctx context.Context, req *storagev1.CreateFileLinkRequest) (*storagev1.CreateFileLinkResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}
	if uf.ExpiredAt != nil {
		return nil, xcodes.ErrFileNotActive.New("file already expired")
	}

	var retainUntil *time.Time
	if ttl := req.GetRetentionTtlSeconds(); ttl > 0 {
		t := time.Now().Add(time.Duration(ttl) * time.Second)
		retainUntil = &t
	}

	// Reuse an existing token: repeated calls are idempotent and renewals
	// must keep links embedded in already-sent emails working.
	token := uf.LinkToken
	if token == nil || *token == "" {
		generated, err := newLinkToken()
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		token = &generated
	}

	if err := dal.UpdateFileLink(ctx, s.db, uf.ID, token, retainUntil); err != nil {
		return nil, err
	}

	resp := &storagev1.CreateFileLinkResponse{LinkToken: *token}
	if retainUntil != nil {
		resp.RetainUntil = retainUntil.Unix()
	} else if uf.RetainUntil != nil {
		resp.RetainUntil = uf.RetainUntil.Unix()
	}
	return resp, nil
}

// GetFileLinkDownload resolves a link token for an anonymous recipient.
// expired=true (with empty download_url) is the "attachment expired" answer
// callers render as a page — not an error, because the link itself is doing
// its job by telling the truth about the file.
func (s *Service) GetFileLinkDownload(ctx context.Context, req *storagev1.GetFileLinkDownloadRequest) (*storagev1.GetFileLinkDownloadResponse, error) {
	uf, err := dal.GetFileByLinkToken(ctx, s.db, req.GetLinkToken())
	if err != nil {
		// Unknown token and expired file render the same way for recipients.
		return &storagev1.GetFileLinkDownloadResponse{Expired: true}, nil
	}

	resp := &storagev1.GetFileLinkDownloadResponse{
		Filename: uf.Filename,
	}
	if uf.RetainUntil != nil {
		resp.RetainUntil = uf.RetainUntil.Unix()
	}

	now := time.Now()
	if uf.ExpiredAt != nil || (uf.RetainUntil != nil && uf.RetainUntil.Before(now)) {
		resp.Expired = true
		return resp, nil
	}

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}
	resp.SizeBytes = obj.Size

	p, err := s.registry.ProviderForBucket(obj.Bucket)
	if err != nil {
		return nil, xcodes.ErrProviderNotFound.Wrap(err)
	}
	var presignOpts []types.GetPresignOption
	if obj.IsPublic {
		presignOpts = append(presignOpts, storage.WithPublic())
	}
	presignOpts = append(presignOpts, storage.WithDownloadFilename(uf.Filename))

	downloadURL, err := p.PresignGetObject(ctx, obj.Bucket, obj.ObjectKey, linkDownloadPresignTTL, presignOpts...)
	if err != nil {
		return nil, fmt.Errorf("presign get object: %w", err)
	}
	resp.DownloadUrl = downloadURL
	return resp, nil
}

// newLinkToken returns a 32-byte random URL-safe token. Random (not derived
// from file state) so it cannot be guessed or invalidated by renewal.
func newLinkToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ReapExpiredFiles runs one two-stage retention-GC pass.
//
// Stage 1 (soft-delete): files past retain_until and not yet marked get
// expired_at = now, plus the same object ref-count decrement and quota
// release a user delete performs — the recovery window must not keep billing
// the owner or holding object references.
//
// Stage 2 (hard-delete): files whose expiry marker is older than
// hardDeleteAfter get their row physically removed. Object bytes and quota
// were already handled at stage 1, so this only clears the tombstone.
//
// Each file is processed in its own transaction; failures are logged and
// retried naturally on the next pass.
func (s *Service) ReapExpiredFiles(ctx context.Context, hardDeleteAfter time.Duration, batchSize int) (*RetentionGCResult, error) {
	if batchSize <= 0 {
		batchSize = 500
	}
	result := &RetentionGCResult{}
	now := time.Now()

	// Stage 1: mark expired (+ ref-count / quota release).
	due, err := dal.FindRetentionDue(ctx, s.db, now, batchSize)
	if err != nil {
		return nil, err
	}
	for _, uf := range due {
		txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			obj, err := dal.GetObjectByID(ctx, tx, uf.ObjectID)
			if err != nil {
				// Object row missing (already reaped elsewhere): still mark
				// the file expired so the tombstone progresses.
				if !errors.Is(err, xcodes.ErrFileNotFound.New()) {
					return err
				}
				obj = nil
			}
			if err := dal.MarkFileExpired(ctx, tx, uf.ID, now); err != nil {
				return err
			}
			if obj != nil {
				if err := dal.DecrObjectRefCount(ctx, tx, obj.ID); err != nil {
					return err
				}
				if err := s.quota.Release(ctx, tx, uf.OwnerType, uf.OwnerID, obj.Size); err != nil {
					return err
				}
			}
			return nil
		})
		if txErr != nil {
			slog.Error("retention gc: mark expired",
				"file_id", uf.ID, "owner_id", uf.OwnerID, "error", txErr)
			continue
		}
		result.MarkedExpired++
	}

	// Stage 2: purge tombstones older than the recovery window.
	purgeCutoff := now.Add(-hardDeleteAfter)
	purgeDue, err := dal.FindPurgeDue(ctx, s.db, purgeCutoff, batchSize)
	if err != nil {
		return result, err
	}
	for _, uf := range purgeDue {
		if err := dal.PurgeFile(ctx, s.db, uf.ID); err != nil {
			slog.Error("retention gc: purge", "file_id", uf.ID, "error", err)
			continue
		}
		result.Purged++
		slog.Info("retention gc: file purged",
			"file_id", uf.ID, "owner_id", uf.OwnerID,
			"expired_at", uf.ExpiredAt.Format(time.RFC3339))
	}
	return result, nil
}
