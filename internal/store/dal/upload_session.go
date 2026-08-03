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

// CreateUploadSession inserts a new session. Dedup of concurrent PENDING
// sessions for the same (owner_type, owner_id, md5, size) is enforced at the
// service layer by a Redis lock (see upload.findOrCreateSession); the DB no
// longer enforces uniqueness on these columns — idx_upload_sessions_pending_dedup
// is a non-unique index kept for FindPendingUploadSessionDedup query performance.
func CreateUploadSession(ctx context.Context, tx *gorm.DB, s *models.StorageUploadSession) error {
	if err := tx.WithContext(ctx).Create(s).Error; err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
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

// (TryUploadSessionAdvisoryLock removed: GC cross-replica exclusion now uses a
// Redis lock in the upload service — see upload.newReaperLock / reap.go. The
// former PostgreSQL advisory lock was the only pg-specific SQL in this package.)
