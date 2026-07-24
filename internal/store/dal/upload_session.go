package dal

import (
	"context"
	"errors"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GetUploadSessionByID returns the active (non-deleted) session by ID, any status.
func GetUploadSessionByID(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageUploadSession, error) {
	s, err := gorm.G[models.StorageUploadSession](tx).
		Where(generated.StorageUploadSession.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrUploadSessionNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &s, nil
}

// FindPendingUploadSessionDedup returns an active PENDING session matching
// (owner_type, owner_id, md5, size) with expiry in the future. Used for
// session reuse on duplicate GetSTSCredential. Returns (nil, false, nil) when
// no candidate is found.
func FindPendingUploadSessionDedup(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64, md5 string, size int64) (*models.StorageUploadSession, bool, error) {
	s, err := gorm.G[models.StorageUploadSession](tx).
		Where(generated.StorageUploadSession.OwnerType.Eq(ownerType)).
		Where(generated.StorageUploadSession.OwnerID.Eq(ownerID)).
		Where(generated.StorageUploadSession.MD5.Eq(md5)).
		Where(generated.StorageUploadSession.Size.Eq(size)).
		Where(generated.StorageUploadSession.Status.Eq(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING))).
		Where(generated.StorageUploadSession.ExpiresAt.Gt(time.Now())).
		Order(generated.StorageUploadSession.ID.Desc()).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, xcodes.ErrInternal.Wrap(err)
	}
	return &s, true, nil
}

// CreateUploadSession inserts a new session. Uses INSERT ... ON CONFLICT
// (owner_type, owner_id, md5, size) DO NOTHING so that two concurrent callers
// racing to create a PENDING session for the same key cannot both succeed —
// the loser hits the partial unique index idx_upload_sessions_pending_dedup
// (scoped to status=PENDING, not-deleted) and RowsAffected comes back as 0.
//
// Returns (inserted, error):
//   - inserted=true: a new row was created; s.ID is populated.
//   - inserted=false: a concurrent caller won the race; the caller MUST
//     re-read the existing session via FindPendingUploadSessionDedup to pick
//     it up.
//
// This is the DB-level backstop for findOrCreateSession when the Redis dedup
// lock is unavailable (Redis down) or simply loses the race (thundering herd).
func CreateUploadSession(ctx context.Context, tx *gorm.DB, s *models.StorageUploadSession) (bool, error) {
	result := tx.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				generated.StorageUploadSession.OwnerType.Column(),
				generated.StorageUploadSession.OwnerID.Column(),
				generated.StorageUploadSession.MD5.Column(),
				generated.StorageUploadSession.Size.Column(),
			},
			DoNothing: true,
		}).
		Create(s)
	if result.Error != nil {
		return false, xcodes.ErrInternal.Wrap(result.Error)
	}
	return result.RowsAffected > 0, nil
}

// MarkUploadSessionConfirmed sets status=CONFIRMED and file_id atomically.
// Returns ErrUploadSessionNotPending when the session is no longer PENDING
// (concurrent confirm or cancel).
func MarkUploadSessionConfirmed(ctx context.Context, tx *gorm.DB, id, fileID int64) error {
	rows, err := gorm.G[models.StorageUploadSession](tx).
		Where(generated.StorageUploadSession.ID.Eq(id)).
		Where(generated.StorageUploadSession.Status.Eq(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING))).
		Set(
			generated.StorageUploadSession.Status.Set(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED)),
			generated.StorageUploadSession.FileID.Set(fileID),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	if rows == 0 {
		return xcodes.ErrUploadSessionNotPending.New()
	}
	return nil
}

// MarkUploadSessionCancelled sets status=CANCELLED atomically. Returns
// ErrUploadSessionNotPending when the session is no longer PENDING.
func MarkUploadSessionCancelled(ctx context.Context, tx *gorm.DB, id int64) error {
	rows, err := gorm.G[models.StorageUploadSession](tx).
		Where(generated.StorageUploadSession.ID.Eq(id)).
		Where(generated.StorageUploadSession.Status.Eq(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING))).
		Set(generated.StorageUploadSession.Status.Set(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CANCELLED))).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	if rows == 0 {
		return xcodes.ErrUploadSessionNotPending.New()
	}
	return nil
}

// MarkUploadSessionExpired atomically transitions a session from PENDING to
// EXPIRED. Returns true when the CAS succeeded (rowsAffected > 0); returns
// false when the row was no longer PENDING (a concurrent confirm/cancel
// already claimed the transition). GC callers must only proceed with the
// cloud delete when this returns true — otherwise deleting the OSS object
// would race with a concurrent confirmUpload that already created file/object
// rows pointing at the same key (silent data loss).
func MarkUploadSessionExpired(ctx context.Context, tx *gorm.DB, id int64) (bool, error) {
	rowsAffected, err := gorm.G[models.StorageUploadSession](tx).
		Where(generated.StorageUploadSession.ID.Eq(id)).
		Where(generated.StorageUploadSession.Status.Eq(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING))).
		Set(generated.StorageUploadSession.Status.Set(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED))).
		Update(ctx)
	if err != nil {
		return false, xcodes.ErrInternal.Wrap(err)
	}
	return rowsAffected > 0, nil
}

// ListExpiredPendingUploadSessions returns up to limit PENDING sessions past
// their expiry, for GC scan.
func ListExpiredPendingUploadSessions(ctx context.Context, tx *gorm.DB, now time.Time, limit int) ([]models.StorageUploadSession, error) {
	sessions, err := gorm.G[models.StorageUploadSession](tx).
		Where(generated.StorageUploadSession.Status.Eq(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING))).
		Where(generated.StorageUploadSession.ExpiresAt.Lt(now)).
		Limit(limit).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return sessions, nil
}

// TryUploadSessionAdvisoryLock attempts to acquire a PostgreSQL session-level
// advisory lock identified by key. Returns (release, acquired, error):
//   - acquired=true: release is a non-nil idempotent func the caller MUST
//     defer; it unlocks and returns the dedicated connection to the pool.
//     The returned error carries any best-effort cleanup failure (unlock or
//     close) — log it, do not propagate.
//   - acquired=false: another backend holds the lock; release is a no-op
//     closure (returns nil) so callers can defer unconditionally.
//
// Session-level (not xact-level) advisory lock: ReapExpiredSessions's work spans OSS
// calls that cannot sit inside a DB transaction. The dedicated connection
// from *sql.DB.Conn pins the lock to one backend for the duration of GC;
// closing the connection auto-releases as a defensive backstop.
//
// HA safety: pg_try_advisory_lock is globally exclusive across all backends,
// so two storage-service replicas running GC cron simultaneously will not
// both process the same batch. Per-row CAS via MarkUploadSessionExpired
// remains the last-line defense against any race window between list and lock
// release.
//
// Note: db is the service's *gorm.DB handle, NOT a transaction. This function
// pulls a dedicated connection out of the underlying *sql.DB pool to pin the
// advisory lock across OSS calls that cannot share a transaction.
func TryUploadSessionAdvisoryLock(ctx context.Context, db *gorm.DB, key int64) (release func() error, acquired bool, err error) {
	pool, gErr := db.DB()
	if gErr != nil {
		return nil, false, xcodes.ErrInternal.Wrap(gErr)
	}
	conn, cErr := pool.Conn(ctx)
	if cErr != nil {
		return nil, false, xcodes.ErrInternal.Wrap(cErr)
	}

	var ok bool
	if qErr := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&ok); qErr != nil {
		closeErr := conn.Close()
		return nil, false, xcodes.ErrInternal.Wrap(errors.Join(qErr, closeErr))
	}

	if !ok {
		closeErr := conn.Close()
		// No lease was acquired; close failure is not actionable. Surface only
		// when it actually happens so the caller can log, but keep acquired=false
		// semantics regardless.
		return func() error { return closeErr }, false, nil
	}

	var released bool
	return func() error {
		if released {
			return nil
		}
		released = true
		var errs []error
		if _, uErr := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", key); uErr != nil {
			errs = append(errs, uErr)
		}
		if cErr := conn.Close(); cErr != nil {
			errs = append(errs, cErr)
		}
		return errors.Join(errs...)
	}, true, nil
}
