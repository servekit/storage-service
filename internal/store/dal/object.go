package dal

import (
	"context"
	"errors"
	"time"

	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
)

// FindObjectByVendorBucketMD5 finds an active storage object by (vendor, bucket, md5).
// Returns (object, true, nil) if found, (nil, false, nil) if not found.
func FindObjectByVendorBucketMD5(ctx context.Context, tx *gorm.DB, vendor int32, bucket, md5 string) (*models.StorageObject, bool, error) {
	obj, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.Vendor.Eq(vendor)).
		Where(generated.StorageObject.Bucket.Eq(bucket)).
		Where(generated.StorageObject.MD5.Eq(md5)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, nil
		}
		return nil, false, xcodes.ErrInternal.Wrap(err)
	}
	return &obj, true, nil
}

// GetObjectByID retrieves an active (non-deleted) storage object by its ID.
//
// Does NOT use the generated GetActiveByID helper: that one is built on
// Raw().Scan(), which on no-rows leaves the struct zero-valued and returns
// nil error (GORM's documented behavior for Scan). We need ErrRecordNotFound
// to map to ErrObjectNotFound, so this builds the query via the typed
// field helpers and uses Take() instead.
func GetObjectByID(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageObject, error) {
	obj, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrObjectNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &obj, nil
}

// BatchGetObjectsByIDs retrieves multiple storage objects by their IDs.
func BatchGetObjectsByIDs(ctx context.Context, tx *gorm.DB, ids []int64) (map[int64]*models.StorageObject, error) {
	if len(ids) == 0 {
		return make(map[int64]*models.StorageObject), nil
	}

	objects, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.ID.In(ids...)).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "batch get by ids")
	}

	result := make(map[int64]*models.StorageObject, len(objects))
	for i := range objects {
		result[objects[i].ID] = &objects[i]
	}
	return result, nil
}

// CreateOrGetObject returns the existing object for (vendor, bucket, md5) or
// inserts a new one. Dedup of concurrent inserts for the same key is serialized
// by a Redis lock in the service layer (see upload.confirmUpload); the DB
// enforces no uniqueness on these columns, keeping the schema portable across
// postgres/mysql/sqlite. Under the lock this check-then-insert is race-free;
// without Redis (lock disabled) two concurrent confirms of identical content
// may create duplicate object rows — accepted for DB portability.
// Returns (object, inserted, error) where inserted indicates a new row was created.
func CreateOrGetObject(ctx context.Context, tx *gorm.DB, obj *models.StorageObject) (*models.StorageObject, bool, error) {
	existing, found, err := FindObjectByVendorBucketMD5(ctx, tx, obj.Vendor, obj.Bucket, obj.MD5)
	if err != nil {
		return nil, false, xcodes.ErrInternal.Wrapf(err, "find existing object")
	}
	if found {
		return existing, false, nil
	}
	if err := tx.WithContext(ctx).Create(obj).Error; err != nil {
		return nil, false, xcodes.ErrInternal.Wrapf(err, "create object")
	}
	return obj, true, nil
}

// IncrObjectRefCount atomically increments the reference count for a storage object.
func IncrObjectRefCount(ctx context.Context, tx *gorm.DB, id int64) error {
	rowsAffected, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.ID.Eq(id)).
		Set(generated.StorageObject.RefCount.Incr(1)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "incr ref count")
	}
	if rowsAffected == 0 {
		return xcodes.ErrObjectNotActive.New()
	}
	return nil
}

// DecrObjectRefCount atomically decrements the reference count for a storage object.
func DecrObjectRefCount(ctx context.Context, tx *gorm.DB, id int64) error {
	return DecrObjectRefCountBy(ctx, tx, id, 1)
}

// DecrObjectRefCountBy atomically decrements the reference count by a given amount.
func DecrObjectRefCountBy(ctx context.Context, tx *gorm.DB, id, count int64) error {
	rowsAffected, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.ID.Eq(id)).
		Where(generated.StorageObject.RefCount.Gte(count)).
		Set(generated.StorageObject.RefCount.Decr(count)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "decr ref count by %d", count)
	}
	if rowsAffected == 0 {
		return xcodes.ErrObjectInsufficientRefCount.New()
	}
	return nil
}

// DeleteObject soft-deletes the active storage object. GORM auto-handles deleted_at
// via the gorm.DeletedAt field on StorageObject — this issues an UPDATE that
// sets deleted_at = now().
// Returns ErrObjectNotActive when the row was already deleted or doesn't exist.
func DeleteObject(ctx context.Context, tx *gorm.DB, id int64) error {
	rowsAffected, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.ID.Eq(id)).
		Delete(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "delete object")
	}
	if rowsAffected == 0 {
		return xcodes.ErrObjectNotActive.New()
	}
	return nil
}

// DeleteZeroRefCountObjects soft-deletes all active objects with zero reference count.
// Returns the number of objects affected.
func DeleteZeroRefCountObjects(ctx context.Context, tx *gorm.DB) (int64, error) {
	rowsAffected, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.RefCount.Eq(0)).
		Delete(ctx)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrapf(err, "delete zero ref count")
	}
	return int64(rowsAffected), nil
}

// FindPurgeableObjects returns storage objects eligible for hard deletion.
// These are soft-deleted objects past the retention period with zero reference count.
//
// Uses Unscoped to bypass the soft-delete filter (otherwise GORM auto-appends
// `WHERE deleted_at IS NULL` and we'd never see the rows we want to purge).
func FindPurgeableObjects(ctx context.Context, tx *gorm.DB, before time.Time) ([]models.StorageObject, error) {
	objects, err := gorm.G[models.StorageObject](tx.Unscoped()).
		Where(generated.StorageObject.DeletedAt.IsNotNull()).
		Where(generated.StorageObject.DeletedAt.Lt(before)).
		Where(generated.StorageObject.RefCount.Eq(0)).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find purgeable")
	}
	return objects, nil
}

// PurgeObject permanently removes a soft-deleted storage object from the database.
// Returns ErrObjectNotSoftDeleted if the object is not in soft-deleted state.
//
// Uses the classic GORM DB API rather than the generic gorm.G[T] helper:
// generic Delete + Unscoped() does not actually hard-delete soft-deletable
// models (returns rowsAffected=0 with nil error), while the classic API
// honors Unscoped and issues a real DELETE.
//
// Single atomic DELETE with WHERE clause: rowsAffected == 0 means the object
// either doesn't exist or isn't soft-deleted, both mapped to ErrObjectNotSoftDeleted.
func PurgeObject(ctx context.Context, tx *gorm.DB, id int64) error {
	result := tx.WithContext(ctx).Unscoped().
		Where("id = ?", id).
		Where("deleted_at IS NOT NULL").
		Delete(&models.StorageObject{})
	if result.Error != nil {
		return xcodes.ErrInternal.Wrapf(result.Error, "purge object")
	}
	if result.RowsAffected == 0 {
		return xcodes.ErrObjectNotSoftDeleted.New()
	}
	return nil
}

// FindObjectByObjectKey finds a storage object by bucket and object key.
func FindObjectByObjectKey(ctx context.Context, tx *gorm.DB, bucket, objectKey string) (*models.StorageObject, error) {
	obj, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.Bucket.Eq(bucket)).
		Where(generated.StorageObject.ObjectKey.Eq(objectKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrObjectNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &obj, nil
}

// BatchFindObjectsByObjectKeys finds storage objects matching any of the given object keys.
func BatchFindObjectsByObjectKeys(ctx context.Context, tx *gorm.DB, bucket string, objectKeys []string) (map[string]*models.StorageObject, error) {
	if len(objectKeys) == 0 {
		return make(map[string]*models.StorageObject), nil
	}

	objects, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.Bucket.Eq(bucket)).
		Where(generated.StorageObject.ObjectKey.In(objectKeys...)).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "batch find object keys")
	}

	result := make(map[string]*models.StorageObject, len(objects))
	for i := range objects {
		result[objects[i].ObjectKey] = &objects[i]
	}
	return result, nil
}

// --- Single-table helpers for service-layer file list filtering ---

// FindObjectIDsByContentTypePrefix returns IDs of active objects whose content type
// starts with prefix. Single-table query on storage_objects.
// Result is capped at MaxObjectIDResults to prevent OOM / Postgres parameter
// limit when used in subsequent IN (...) clauses. Callers needing more should
// paginate.
func FindObjectIDsByContentTypePrefix(ctx context.Context, tx *gorm.DB, prefix string) ([]int64, error) {
	objects, err := gorm.G[models.StorageObject](tx).
		Where(generated.StorageObject.ContentType.Like(prefix + "%")).
		Limit(MaxObjectIDResults).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by content type prefix")
	}
	ids := make([]int64, len(objects))
	for i, o := range objects {
		ids[i] = o.ID
	}
	return ids, nil
}

// FindObjectIDsByFilter returns IDs of active objects matching the optional filters.
// Zero-value filters are ignored. Single-table query on storage_objects.
// Result is capped at MaxObjectIDResults to prevent OOM / Postgres parameter
// limit when used in subsequent IN (...) clauses. Callers needing more should
// paginate.
func FindObjectIDsByFilter(ctx context.Context, tx *gorm.DB, contentTypePrefix string, vendor int32, bucket string) ([]int64, error) {
	// gorm.G[T](tx) returns Interface[T], but q is reassigned with conditional
	// Where clauses (which return ChainInterface[T]). Use a no-op Scopes to
	// bridge to ChainInterface[T] without adding a real filter — GORM's
	// soft-delete auto-filter still applies on the resulting query.
	q := gorm.G[models.StorageObject](tx).Scopes(func(*gorm.Statement) {})
	if contentTypePrefix != "" {
		q = q.Where(generated.StorageObject.ContentType.Like(contentTypePrefix + "%"))
	}
	if vendor != 0 {
		q = q.Where(generated.StorageObject.Vendor.Eq(vendor))
	}
	if bucket != "" {
		q = q.Where(generated.StorageObject.Bucket.Eq(bucket))
	}
	objects, err := q.Limit(MaxObjectIDResults).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by filter")
	}
	ids := make([]int64, len(objects))
	for i, o := range objects {
		ids[i] = o.ID
	}
	return ids, nil
}

// --- Single-table aggregation helpers for service-layer stats ---

// CountActiveAndSumObjectSizeByIDs returns count and total size of active objects
// matching the given IDs. Single-table aggregation on storage_objects.
func CountActiveAndSumObjectSizeByIDs(ctx context.Context, tx *gorm.DB, ids []int64) (models.PhysicalStatsRow, error) {
	if len(ids) == 0 {
		return models.PhysicalStatsRow{}, nil
	}
	result, err := generated.StorageObjectQuery[models.StorageObject](tx).CountActiveAndSumSize(ctx, ids)
	if err != nil {
		return models.PhysicalStatsRow{}, xcodes.ErrInternal.Wrapf(err, "count and sum size by ids")
	}
	return result, nil
}

// GroupObjectsByVendorAndSumSize groups active objects matching the IDs by
// vendor, returning count and total size per vendor. Single-table aggregation
// on storage_objects.
func GroupObjectsByVendorAndSumSize(ctx context.Context, tx *gorm.DB, ids []int64) ([]models.ProviderStatRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	result, err := generated.StorageObjectQuery[models.StorageObject](tx).GroupByVendorCountAndSumSize(ctx, ids)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "group by vendor and sum size")
	}
	return result, nil
}

// GroupObjectsByBucketAndSumSize groups active objects matching the IDs by
// bucket, returning count and total size per bucket. Single-table aggregation
// on storage_objects.
func GroupObjectsByBucketAndSumSize(ctx context.Context, tx *gorm.DB, ids []int64) ([]models.BucketObjectStatRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	result, err := generated.StorageObjectQuery[models.StorageObject](tx).GroupByBucketCountAndSumSize(ctx, ids)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "group by bucket and sum size")
	}
	return result, nil
}
