package upload

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/servekit/go-common/redisx"
	"github.com/servekit/go-common/xerr"
	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/pkg/xcodes"
)

// ReapExpiredSessions scans one batch of expired PENDING sessions and cleans
// up OSS orphans. Pure logic — caller (jobs.Scheduler, test) decides when to
// invoke. Returns the count of orphan objects deleted from cloud storage.
//
// HA safety: acquires a cross-replica Redis lock (replaces the former
// PostgreSQL advisory lock) before scanning so two replicas running the reap
// job concurrently do not both process the same batch. If Redis is unavailable
// (reaperLock nil) or the lock is held, it either proceeds lock-free or skips
// the cycle. Correctness is independent of this lock — the per-row CAS in
// MarkUploadSessionExpired is the last-line defense.
func (s *Service) ReapExpiredSessions(ctx context.Context) (int, error) {
	if s.lock != nil {
		lockID, lockErr := s.lock.Acquire(ctx, lockTargetReap)
		if lockErr != nil {
			if errors.Is(lockErr, redisx.ErrLockFailed) {
				slog.Info("upload gc: lease held by another replica, skipping cycle")
				return 0, nil
			}
			return 0, xcodes.ErrInternal.Wrapf(lockErr, "acquire upload gc lock")
		}
		// GC spans OSS Head/Delete across a batch and can outlive the shared
		// lock TTL; KeepAlive renews it (at TTL/3) until ReapExpiredSessions
		// returns, so the TTL acts only as the crash-recovery window.
		cancelKeepAlive := s.lock.KeepAlive(ctx, lockTargetReap, lockID)
		defer cancelKeepAlive()
		defer func() {
			if relErr := s.lock.Release(context.Background(), lockTargetReap, lockID); relErr != nil {
				slog.Warn("upload gc: release redis gc lock", "error", relErr)
			}
		}()
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

	// Phase 2: settled sessions (CONFIRMED / CANCELLED) past expiry. Their
	// staging object is exclusively owned — no live file row points at it (the
	// confirmed bytes live at the content-addressed final key) — so reclaiming
	// it is always safe. This bounds the leak from a failed inline staging
	// delete in ConfirmUpload and collects cancelled sessions' leftovers.
	settled, err := dal.ListExpiredSettledUploadSessions(ctx, s.db, now, batchSize)
	if err != nil {
		// Phase 1 already succeeded; a phase-2 scan failure should not fail
		// the whole reap call (the next cycle retries).
		slog.Error("upload gc: list settled sessions", "error", err)
		return deleted, nil
	}
	for i := range settled {
		sess := settled[i]

		// Only staging keys (owner-sandbox minted) are exclusively owned by the
		// session and safe to delete. Legacy rows carry the content-addressed
		// key directly (globally deduped, possibly shared with live objects) —
		// leave both the object and the row untouched; they are one deploy's
		// worth of history and re-scanning them is harmless.
		bucketCfg, cfgErr := s.registry.BucketConfig(sess.Bucket)
		if cfgErr != nil {
			slog.Error("upload gc: resolve bucket config for settled session", "session_id", sess.ID, "bucket", sess.Bucket, "error", cfgErr)
			continue
		}
		if !conv.IsSandboxObjectKey(sess.ObjectKey, bucketCfg.KeyPrefix, sess.OwnerType, sess.OwnerID) {
			continue
		}
		p, pErr := s.registry.ProviderForBucket(sess.Bucket)
		if pErr != nil {
			slog.Error("upload gc: resolve provider for settled session", "session_id", sess.ID, "bucket", sess.Bucket, "error", pErr)
			continue
		}
		// Head-first so an already-reclaimed staging object (provider
		// DeleteObject semantics vary on missing keys) still lets the row
		// retire instead of being rescanned forever.
		if _, headErr := p.HeadObject(ctx, sess.Bucket, sess.ObjectKey); headErr != nil {
			if !errors.Is(headErr, storage.ErrObjectNotFound) {
				slog.Error("upload gc: head settled staging object", "session_id", sess.ID, "key", sess.ObjectKey, "error", headErr)
				continue
			}
		} else if delErr := p.DeleteObject(ctx, sess.Bucket, sess.ObjectKey); delErr != nil {
			slog.Error("upload gc: delete settled staging object", "session_id", sess.ID, "key", sess.ObjectKey, "error", delErr)
			continue
		} else {
			deleted++
		}
		if err := dal.SoftDeleteUploadSession(ctx, s.db, sess.ID); err != nil {
			// Not-pending here means a concurrent PENDING-path transition —
			// skip quietly; anything else logs and retries next cycle.
			var xe *xerr.Error
			if errors.As(err, &xe) && xe.Code().Reason() == xcodes.ErrUploadSessionNotPending.Reason() {
				continue
			}
			slog.Error("upload gc: retire settled session", "session_id", sess.ID, "error", err)
			continue
		}
	}
	return deleted, nil
}
