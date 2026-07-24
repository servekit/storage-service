package upload

import (
	"context"
	"errors"
	"log/slog"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/store/dal"
)

// reapAdvisoryLockKey is the PostgreSQL advisory-lock key identifying the
// upload-session reap lease. Two HA replicas running the reap job
// simultaneously will contend on this single key; only one acquires and
// proceeds per cycle. The value 0x534F4C47 ("SOLG") is arbitrary but
// stable; tests pin the same value (see internal/store/dal/upload_session_test.go).
const reapAdvisoryLockKey int64 = 0x534F4C47

// ReapExpiredSessions scans one batch of expired PENDING sessions and cleans
// up OSS orphans. Pure logic — caller (jobs.Scheduler, test) decides when to
// invoke. Returns the count of orphan objects deleted from cloud storage.
//
// HA safety: acquires a session-level advisory lock before scanning so that
// two replicas running the reap job concurrently do not both process the same
// batch. If the lock cannot be acquired (another replica holds it), returns
// (0, nil) without touching OSS.
func (s *Service) ReapExpiredSessions(ctx context.Context) (int, error) {
	release, acquired, err := dal.TryUploadSessionAdvisoryLock(ctx, s.db, reapAdvisoryLockKey)
	if err != nil {
		return 0, err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			slog.Warn("upload gc: release advisory lock lease", "error", relErr)
		}
	}()
	if !acquired {
		slog.Info("upload gc: lease held by another replica, skipping cycle")
		return 0, nil
	}

	now := time.Now()
	batchSize := s.cfg.Storage.UploadGC.BatchSize

	sessions, err := dal.ListExpiredPendingUploadSessions(ctx, s.db, now, batchSize)
	if err != nil {
		return 0, err
	}

	deleted := 0
	for i := range sessions {
		sess := sessions[i]

		p, err := s.registry.ProviderForBucket(sess.Bucket)
		if err != nil {
			slog.Error("upload gc: resolve provider", "session_id", sess.ID, "bucket", sess.Bucket, "error", err)
			// Mark expired anyway so we don't keep retrying a session whose bucket is gone.
			if _, markErr := dal.MarkUploadSessionExpired(ctx, s.db, sess.ID); markErr != nil {
				slog.Error("upload gc: mark expired", "session_id", sess.ID, "error", markErr)
			}
			continue
		}

		info, headErr := p.HeadObject(ctx, sess.Bucket, sess.ObjectKey)
		switch {
		case headErr != nil && errors.Is(headErr, storage.ErrObjectNotFound):
			// Client never uploaded successfully. Safe to expire without
			// deleting — there is nothing in OSS to reclaim.
		case headErr != nil:
			// Transient error (network blip, OSS 5xx, timeout): the object
			// MIGHT exist as an orphan. Skip this cycle without MarkExpired so
			// the next GC run re-evaluates once OSS is reachable again.
			slog.Error("upload gc: head object transient error", "session_id", sess.ID, "key", sess.ObjectKey, "error", headErr)
			continue
		case info != nil:
			// Orphan — client uploaded but never confirmed. Atomically claim
			// the session via CAS (PENDING → EXPIRED) BEFORE deleting OSS data.
			// If the CAS fails (rowsAffected == 0), a concurrent confirmUpload
			// or cancelUpload already transitioned the session out of PENDING —
			// deleting now would orphan the freshly-created file/object rows
			// and cause silent data loss. Skip and let next cycle reconcile.
			claimed, markErr := dal.MarkUploadSessionExpired(ctx, s.db, sess.ID)
			if markErr != nil {
				slog.Error("upload gc: mark expired", "session_id", sess.ID, "error", markErr)
				continue
			}
			if !claimed {
				slog.Info("upload gc: session transitioned concurrently, skipping cloud delete",
					"session_id", sess.ID, "key", sess.ObjectKey)
				continue
			}

			// We own the PENDING → EXPIRED transition; safe to reclaim OSS.
			if delErr := p.DeleteObject(ctx, sess.Bucket, sess.ObjectKey); delErr != nil {
				slog.Error("upload gc: delete orphan", "session_id", sess.ID, "key", sess.ObjectKey, "error", delErr)
				// Session is already EXPIRED; the OSS object may linger as a
				// post-expire orphan. Reclaiming those is out of ReapExpiredSessions's
				// current scope (it scans PENDING only).
				continue
			}
			deleted++
		default:
			// headErr == nil && info == nil: provider signalled not-found via
			// the (nil, nil) shape. Treat as never-uploaded and expire.
		}

		// For the not-found branches above, claim the session via CAS so we
		// don't clobber a concurrent confirm/cancel. If the CAS fails, the
		// session was already transitioned — nothing more to do.
		claimed, markErr := dal.MarkUploadSessionExpired(ctx, s.db, sess.ID)
		if markErr != nil {
			slog.Error("upload gc: mark expired", "session_id", sess.ID, "error", markErr)
			continue
		}
		if !claimed {
			slog.Info("upload gc: session transitioned concurrently during not-found cleanup",
				"session_id", sess.ID)
			continue
		}

		s.host.RecordOutcome(ctx, AuditEvent{
			Action:     storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_GC,
			OwnerType:  sess.OwnerType,
			OwnerID:    sess.OwnerID,
			TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
			TargetID:   sess.ID,
			Before: conv.MustToMap(SessionSnapshot{
				ID:     sess.ID,
				Status: int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
			}),
			After: conv.MustToMap(SessionSnapshot{
				ID:     sess.ID,
				Status: int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED),
			}),
		}, nil)
	}
	return deleted, nil
}
