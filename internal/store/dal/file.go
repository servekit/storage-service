package dal

import (
	"context"
	"errors"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// ListFilesFilter defines filtering and pagination options for listing files.
//
// Cursor semantics: ListFilesByOwner / ListAllFiles order by (sort_col, id).
// To page without dropping or duplicating rows when sort_col != id, callers
// must supply BOTH AfterID (the tiebreaker) AND the matching After* sort
// column from the previous page's last row. The DAL emits a row-value
// comparison so the cursor advances past the entire sort tuple.
type ListFilesFilter struct {
	PathPrefix        string
	Extension         string
	ContentTypePrefix string
	OrderBy           storagev1.SortField
	Descending        bool
	AfterFilename     string
	AfterCreatedAt    time.Time
	dbx.Pagination
}

// ListFilesPagedFilter defines filtering + offset pagination options for
// ListFilesByOwnerPaged. Use this for UI page-jump flows where total_count
// and page numbers are required.
//
// ContentTypePrefix is intentionally absent: the service layer resolves it
// to a list of object IDs (cross-table query) and passes that via the
// objectIDs parameter, matching ListFilesFilter's contract.
type ListFilesPagedFilter struct {
	PathPrefix string
	Extension  string
	OrderBy    storagev1.SortField
	Descending bool
	dbx.PageParams
}

// AdminListFilesFilter defines filtering and pagination options for admin file listing.
// All filter fields are optional — zero values mean "no filter".
// Cursor semantics match ListFilesFilter.
type AdminListFilesFilter struct {
	OwnerType         int32
	OwnerID           int64
	PathPrefix        string
	Extension         string
	ContentTypePrefix string
	Vendor            int32 // 0 = no filter
	Bucket            string
	OrderBy           storagev1.SortField
	Descending        bool
	AfterFilename     string
	AfterCreatedAt    time.Time
	dbx.Pagination
}

// CreateFile inserts a new file record.
func CreateFile(ctx context.Context, tx *gorm.DB, f *models.StorageFile) error {
	if err := gorm.G[models.StorageFile](tx).Create(ctx, f); err != nil {
		return xcodes.ErrInternal.Wrapf(err, "create file")
	}
	return nil
}

// GetFileByIDAndOwner retrieves a file by ID and owner (ownership check).
func GetFileByIDAndOwner(ctx context.Context, tx *gorm.DB, id, ownerID int64, ownerType int32) (*models.StorageFile, error) {
	f, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.ID.Eq(id)).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Where(generated.StorageFile.OwnerType.Eq(ownerType)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrFileNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &f, nil
}

// GetFileByID retrieves a file by ID without owner check (admin use).
func GetFileByID(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageFile, error) {
	f, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrFileNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &f, nil
}

// ListFilesByOwner returns a cursor-paginated list of files for a given owner.
// No COUNT(*) is performed — this is a pure cursor for stable iteration. Use
// ListFilesByOwnerPaged when total_count is needed.
//
// objectIDs semantics:
//   - nil: no object_id filter applied
//   - empty slice []int64{}: caller already determined no objects match,
//     return empty result without hitting DB
//   - non-empty: WHERE object_id IN (...) filter
//
// ContentTypePrefix in filter is ignored; service layer resolves it.
func ListFilesByOwner(ctx context.Context, tx *gorm.DB, ownerID int64, ownerType int32, filter ListFilesFilter, objectIDs []int64) ([]models.StorageFile, error) {
	if len(objectIDs) == 0 && objectIDs != nil {
		return nil, nil
	}

	q := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Where(generated.StorageFile.OwnerType.Eq(ownerType))

	if filter.PathPrefix != "" {
		q = q.Where(generated.StorageFile.FilePath.Like(filter.PathPrefix + "%"))
	}
	if filter.Extension != "" {
		q = q.Where(generated.StorageFile.Filename.Like("%." + filter.Extension))
	}
	if len(objectIDs) > 0 {
		q = q.Where(generated.StorageFile.ObjectID.In(objectIDs...))
	}

	q = applyFileOrder(q, filter.OrderBy, filter.Descending)

	pg := filter.Normalize()

	q = applyFileCursor(q, filter.OrderBy, filter.Descending, pg.AfterID, filter.AfterFilename, filter.AfterCreatedAt)

	files, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "list files")
	}

	return files, nil
}

// ListFilesByOwnerPaged returns a page of files for an owner using offset
// pagination, plus the total count of matching rows. Use this for UI list
// views that need page numbers and total counts. For stable iteration under
// concurrent writes, use ListFilesByOwner (cursor) instead.
//
// objectIDs semantics match ListFilesByOwner:
//   - nil: no object_id filter applied
//   - empty slice []int64{}: short-circuit, return (nil, 0, nil)
//   - non-empty: WHERE object_id IN (...)
//
// ContentTypePrefix in filter is ignored; service layer must resolve it.
//
// filter.Count controls whether COUNT(*) is run; set false to skip the total
// calculation (the returned total will be 0 in that case).
func ListFilesByOwnerPaged(ctx context.Context, tx *gorm.DB, ownerID int64, ownerType int32, filter ListFilesPagedFilter, objectIDs []int64) ([]models.StorageFile, int64, error) {
	if len(objectIDs) == 0 && objectIDs != nil {
		return nil, 0, nil
	}

	q := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Where(generated.StorageFile.OwnerType.Eq(ownerType))

	if filter.PathPrefix != "" {
		q = q.Where(generated.StorageFile.FilePath.Like(filter.PathPrefix + "%"))
	}
	if filter.Extension != "" {
		q = q.Where(generated.StorageFile.Filename.Like("%." + filter.Extension))
	}
	if len(objectIDs) > 0 {
		q = q.Where(generated.StorageFile.ObjectID.In(objectIDs...))
	}

	var (
		total int64
		files []models.StorageFile
		err   error
	)

	if filter.Count {
		total, err = q.Count(ctx, "*")
		if err != nil {
			return nil, 0, xcodes.ErrInternal.Wrapf(err, "count files (paged)")
		}
	}

	q = applyFileOrder(q, filter.OrderBy, filter.Descending)

	pp := filter.Normalize()
	offset := (pp.Page - 1) * pp.PageSize
	if offset > 0 {
		q = q.Offset(offset)
	}
	files, err = q.Limit(pp.PageSize).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list files (paged)")
	}

	return files, total, nil
}

// ListAllFiles returns a paginated list of all files with optional filters (admin use).
//
// objectIDs semantics same as ListFilesByOwner.
// ContentTypePrefix/Vendor/Bucket in filter are ignored by this method.
func ListAllFiles(ctx context.Context, tx *gorm.DB, filter AdminListFilesFilter, objectIDs []int64) ([]models.StorageFile, int, error) {
	if len(objectIDs) == 0 && objectIDs != nil {
		return nil, 0, nil
	}

	// gorm.G[T](tx) returns Interface[T], but q is reassigned with conditional
	// Where/Order clauses (which return ChainInterface[T]). Use a no-op Scopes
	// to bridge to ChainInterface[T] without adding a real filter — GORM's
	// soft-delete auto-filter still applies on the resulting query.
	q := gorm.G[models.StorageFile](tx).Scopes(func(*gorm.Statement) {})

	if filter.OwnerType > 0 {
		q = q.Where(generated.StorageFile.OwnerType.Eq(filter.OwnerType))
	}
	if filter.OwnerID > 0 {
		q = q.Where(generated.StorageFile.OwnerID.Eq(filter.OwnerID))
	}
	if filter.PathPrefix != "" {
		q = q.Where(generated.StorageFile.FilePath.Like(filter.PathPrefix + "%"))
	}
	if filter.Extension != "" {
		q = q.Where(generated.StorageFile.Filename.Like("%." + filter.Extension))
	}
	if len(objectIDs) > 0 {
		q = q.Where(generated.StorageFile.ObjectID.In(objectIDs...))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count files (admin)")
	}

	q = applyFileOrder(q, filter.OrderBy, filter.Descending)

	pg := filter.Normalize()

	q = applyFileCursor(q, filter.OrderBy, filter.Descending, pg.AfterID, filter.AfterFilename, filter.AfterCreatedAt)

	files, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list files (admin)")
	}

	return files, int(total), nil
}

// applyFileOrder applies the ORDER BY clause shared by cursor and offset
// file-listing paths. The sort is always (sort_col, id) so pagination is
// stable and row dedup is impossible.
//
// SIZE intentionally falls back to created_at: file size lives on
// StorageObject, not StorageFile, so we can't sort by it without a join.
// Callers needing size-order must join separately; this keeps the helper
// single-table.
func applyFileOrder(q gorm.ChainInterface[models.StorageFile], orderBy storagev1.SortField, descending bool) gorm.ChainInterface[models.StorageFile] {
	switch orderBy {
	case storagev1.SortField_SORT_FIELD_FILENAME:
		if descending {
			return q.Order(generated.StorageFile.Filename.Desc()).Order(generated.StorageFile.ID.Desc())
		}
		return q.Order(generated.StorageFile.Filename).Order(generated.StorageFile.ID)
	default:
		// SIZE and UNSPECIFIED both fall through to created_at.
		if descending {
			return q.Order(generated.StorageFile.CreatedAt.Desc()).Order(generated.StorageFile.ID.Desc())
		}
		return q.Order(generated.StorageFile.CreatedAt).Order(generated.StorageFile.ID)
	}
}

// applyFileCursor advances a file query past the (sort_col, id) tuple of the
// last row from the previous page. This is the only way to page safely when
// ORDER BY uses a non-id column: a bare `id < afterID` cursor drops rows
// whose id ordering disagrees with the sort column (the common case).
//
// Callers must pass the sort-column value (afterFilename or afterCreatedAt)
// alongside afterID; if only afterID is set the cursor degrades to the
// legacy `id < afterID` form (correct only when ORDER BY is id-descending).
func applyFileCursor(q gorm.ChainInterface[models.StorageFile], orderBy storagev1.SortField, descending bool, afterID int64, afterFilename string, afterCreatedAt time.Time) gorm.ChainInterface[models.StorageFile] {
	if afterID == 0 {
		return q
	}
	switch orderBy {
	case storagev1.SortField_SORT_FIELD_FILENAME:
		if afterFilename == "" {
			return q
		}
		if descending {
			return q.Where("filename < ? OR (filename = ? AND id < ?)", afterFilename, afterFilename, afterID)
		}
		return q.Where("filename > ? OR (filename = ? AND id > ?)", afterFilename, afterFilename, afterID)
	default:
		// CREATED_AT (and any UNSPECIFIED fallback). Fall back to id-only
		// when the caller didn't supply a timestamp (legacy callers).
		if afterCreatedAt.IsZero() {
			if descending {
				return q.Where("id < ?", afterID)
			}
			return q.Where("id > ?", afterID)
		}
		if descending {
			return q.Where("created_at < ? OR (created_at = ? AND id < ?)", afterCreatedAt, afterCreatedAt, afterID)
		}
		return q.Where("created_at > ? OR (created_at = ? AND id > ?)", afterCreatedAt, afterCreatedAt, afterID)
	}
}

// UpdateFile saves changes to an existing file.
func UpdateFile(ctx context.Context, tx *gorm.DB, f *models.StorageFile) error {
	result := tx.WithContext(ctx).
		Where(generated.StorageFile.ID.Eq(f.ID)).
		Where(generated.StorageFile.OwnerID.Eq(f.OwnerID)).
		Select("*").
		Updates(f)
	if result.Error != nil {
		return xcodes.ErrInternal.Wrapf(result.Error, "update file")
	}
	if result.RowsAffected == 0 {
		return xcodes.ErrFileNotActive.New()
	}
	return nil
}

// DeleteFile soft-deletes a file by id. GORM auto-handles deleted_at via the
// gorm.DeletedAt field on File.
// Returns ErrFileNotActive when the row was already deleted or doesn't exist.
func DeleteFile(ctx context.Context, tx *gorm.DB, id int64) error {
	rowsAffected, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.ID.Eq(id)).
		Delete(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "delete file")
	}
	if rowsAffected == 0 {
		return xcodes.ErrFileNotActive.New()
	}
	return nil
}

// BatchDeleteFiles soft-deletes multiple files owned by the given owner.
// Returns the number of files actually deleted.
func BatchDeleteFiles(ctx context.Context, tx *gorm.DB, ids []int64, ownerID int64, ownerType int32) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > MaxBatchSize {
		return 0, xcodes.ErrFileBatchTooLarge.New()
	}

	rowsAffected, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.ID.In(ids...)).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Where(generated.StorageFile.OwnerType.Eq(ownerType)).
		Delete(ctx)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrapf(err, "batch delete")
	}
	return rowsAffected, nil
}

// CountFilesByOwner returns the number of active files for an owner.
func CountFilesByOwner(ctx context.Context, tx *gorm.DB, ownerID int64, ownerType int32) (int64, error) {
	count, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Where(generated.StorageFile.OwnerType.Eq(ownerType)).
		Count(ctx, "*")
	if err != nil {
		return 0, xcodes.ErrInternal.Wrapf(err, "count owner files")
	}
	return count, nil
}

// GetFileObjectRefCountsByOwner returns a map of objectID -> file count for all
// active files belonging to the given owner. Used for batch ref count decrements.
func GetFileObjectRefCountsByOwner(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64) (map[int64]int64, error) {
	rows, err := generated.StorageFileQuery[models.StorageFile](tx).GetObjectRefCounts(ctx, ownerType, ownerID)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "get object ref counts")
	}

	counts := make(map[int64]int64, len(rows))
	for _, row := range rows {
		counts[row.ObjectID] = row.Count
	}
	return counts, nil
}

// DeleteFilesByOwner soft-deletes all active files for an owner. Returns count of affected rows.
func DeleteFilesByOwner(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64) (int64, error) {
	rowsAffected, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.OwnerType.Eq(ownerType)).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Delete(ctx)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrapf(err, "delete by owner")
	}
	return int64(rowsAffected), nil
}

// --- Single-table helpers for service-layer stats composition ---

// FindFileObjectIDsByOwner returns object_ids referenced by active files of the
// given owner. May contain duplicates (multiple files per object).
// Single-table query on files.
func FindFileObjectIDsByOwner(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64) ([]int64, error) {
	files, err := gorm.G[models.StorageFile](tx).
		Where(generated.StorageFile.OwnerType.Eq(ownerType)).
		Where(generated.StorageFile.OwnerID.Eq(ownerID)).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by owner")
	}
	ids := make([]int64, len(files))
	for i, f := range files {
		ids[i] = f.ObjectID
	}
	return ids, nil
}

// FindFileOwnerObjectIDPairs returns (owner_type, object_id) pairs for all active
// files. Single-table query on files. Used by service layer to compute
// OwnerStats by composing FileRepo results with ObjectRepo size lookups.
// Result is capped at MaxObjectIDResults to prevent OOM / Postgres parameter
// limit when used in subsequent IN (...) clauses. Callers needing more should
// paginate.
//
// Order by id: the cap means we only see a prefix of the active rows. Pinning
// the order to id makes the result deterministic across calls (same snapshot
// → same rows), so stats computations are reproducible. Without ORDER BY,
// Postgres is free to return any 10000 rows, which makes "missing data" bugs
// intermittent and hard to diagnose.
func FindFileOwnerObjectIDPairs(ctx context.Context, tx *gorm.DB) ([]models.OwnerObjectIDPair, error) {
	pairs, err := generated.StorageFileQuery[models.StorageFile](tx).FindOwnerObjectIDPairs(ctx, MaxObjectIDResults)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find owner object pairs")
	}
	return pairs, nil
}
