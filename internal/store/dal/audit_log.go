package dal

import (
	"context"
	"time"

	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// AuditLogFilter defines filtering and pagination options for listing audit logs.
type AuditLogFilter struct {
	OwnerType  int32
	OwnerID    int64
	TargetType int32
	TargetID   int64
	Action     int32
	Status     int32
	RequestID  string
	StartTime  time.Time
	EndTime    time.Time
	dbx.Pagination
}

// CreateAuditLog inserts an audit log record.
func CreateAuditLog(ctx context.Context, tx *gorm.DB, log *models.StorageAuditLog) error {
	if err := gorm.G[models.StorageAuditLog](tx).Create(ctx, log); err != nil {
		return xcodes.ErrInternal.Wrapf(err, "create audit log")
	}
	return nil
}

// ListAuditLogsByOwner returns a paginated list of audit logs for a given owner.
func ListAuditLogsByOwner(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64, filter AuditLogFilter) ([]models.StorageAuditLog, int, error) {
	q := gorm.G[models.StorageAuditLog](tx).
		Where(generated.StorageAuditLog.OwnerType.Eq(ownerType)).
		Where(generated.StorageAuditLog.OwnerID.Eq(ownerID))

	if filter.Action != 0 {
		q = q.Where(generated.StorageAuditLog.Action.Eq(filter.Action))
	}
	if filter.TargetType != 0 {
		q = q.Where(generated.StorageAuditLog.TargetType.Eq(filter.TargetType))
	}
	if !filter.StartTime.IsZero() {
		q = q.Where(generated.StorageAuditLog.CreatedAt.Gte(filter.StartTime))
	}
	if !filter.EndTime.IsZero() {
		q = q.Where(generated.StorageAuditLog.CreatedAt.Lte(filter.EndTime))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count audit logs")
	}

	q = q.Order(generated.StorageAuditLog.CreatedAt.Desc()).Order(generated.StorageAuditLog.ID.Desc())

	pg := filter.Normalize()
	if pg.AfterID > 0 {
		q = q.Where(generated.StorageAuditLog.ID.Lt(pg.AfterID))
	}

	logs, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list audit logs")
	}

	return logs, int(total), nil
}

// ListAllAuditLogs returns a paginated list of all audit logs with filters (admin use).
func ListAllAuditLogs(ctx context.Context, tx *gorm.DB, filter AuditLogFilter) ([]models.StorageAuditLog, int, error) {
	// Bootstrap q with a no-op predicate so the chain has type ChainInterface[T].
	// Without this, conditional .Where() calls would need a separate base query each time.
	q := gorm.G[models.StorageAuditLog](tx).Where(generated.StorageAuditLog.ID.Gt(0))

	if filter.OwnerType > 0 {
		q = q.Where(generated.StorageAuditLog.OwnerType.Eq(filter.OwnerType))
	}
	if filter.OwnerID > 0 {
		q = q.Where(generated.StorageAuditLog.OwnerID.Eq(filter.OwnerID))
	}
	if filter.TargetType != 0 {
		q = q.Where(generated.StorageAuditLog.TargetType.Eq(filter.TargetType))
	}
	if filter.TargetID > 0 {
		q = q.Where(generated.StorageAuditLog.TargetID.Eq(filter.TargetID))
	}
	if filter.Action != 0 {
		q = q.Where(generated.StorageAuditLog.Action.Eq(filter.Action))
	}
	if filter.Status != 0 {
		q = q.Where(generated.StorageAuditLog.Status.Eq(filter.Status))
	}
	if filter.RequestID != "" {
		q = q.Where(generated.StorageAuditLog.RequestID.Eq(filter.RequestID))
	}
	if !filter.StartTime.IsZero() {
		q = q.Where(generated.StorageAuditLog.CreatedAt.Gte(filter.StartTime))
	}
	if !filter.EndTime.IsZero() {
		q = q.Where(generated.StorageAuditLog.CreatedAt.Lte(filter.EndTime))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count audit logs (admin)")
	}

	q = q.Order(generated.StorageAuditLog.CreatedAt.Desc()).Order(generated.StorageAuditLog.ID.Desc())

	pg := filter.Normalize()
	if pg.AfterID > 0 {
		q = q.Where(generated.StorageAuditLog.ID.Lt(pg.AfterID))
	}

	logs, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list audit logs (admin)")
	}

	return logs, int(total), nil
}
