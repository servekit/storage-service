package admin

import (
	"context"

	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"
)

// globalStats holds aggregate storage statistics returned to admin callers.
type globalStats struct {
	TotalObjects  int64
	PhysicalBytes int64
	TotalFiles    int64
	LogicalBytes  int64
	OwnerStats    []ownerStat
	ProviderStats []providerStat
	BucketStats   []bucketStat
}

// ownerStat is the wire-format per-owner-type aggregate.
type ownerStat struct {
	OwnerType  int32
	FileCount  int64
	TotalBytes int64
}

// providerStat is the wire-format per-provider aggregate.
type providerStat struct {
	Vendor      int32
	ObjectCount int64
	TotalBytes  int64
}

// bucketStat is the wire-format per-bucket aggregate.
type bucketStat struct {
	Bucket      string
	ObjectCount int64
	TotalBytes  int64
	FileCount   int64
}

// getStorageStats computes aggregate storage statistics by composing
// single-table repository methods. Replaces the old ObjectRepo.GetStats.
//
// Composition:
//   - TotalFiles: StorageFileQuery.GetFileCount (single-table generated query)
//   - LogicalBytes: StorageQuotaQuery.GetUsedBytes / GetTotalUsedBytes (single-table)
//   - TotalObjects + PhysicalBytes: dal.CountActiveAndSumObjectSizeByIDs
//     (when owner-filtered) — composed with FileRepo.FindObjectIDsByOwner
//   - OwnerStats: FileRepo.FindOwnerObjectIDPairs + dal.BatchGetObjectsByIDs,
//     aggregated in memory by owner_type
//   - ProviderStats: dal.GroupObjectsByVendorAndSumSize
//   - BucketStats: dal.GroupObjectsByBucketAndSumSize + per-bucket
//     file count composed from FileRepo + ObjectRepo data
func (s *Service) getStorageStats(ctx context.Context, ownerType int32, ownerID int64) (*globalStats, error) {
	stats := &globalStats{}

	// 1. File count (single-table via generated).
	fileCountRow, err := generated.StorageFileQuery[models.StorageFile](s.db).GetFileCount(ctx, ownerType, ownerID)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "get file count")
	}
	stats.TotalFiles = fileCountRow.Count

	// 2. Used bytes (single-table via generated).
	if ownerType > 0 && ownerID > 0 {
		used, err := generated.StorageQuotaQuery[models.StorageQuota](s.db).GetUsedBytes(ctx, ownerType, ownerID)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "get used bytes")
		}
		stats.LogicalBytes = used.UsedBytes
	} else if ownerType <= 0 {
		totalUsed, err := generated.StorageQuotaQuery[models.StorageQuota](s.db).GetTotalUsedBytes(ctx)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "get total used bytes")
		}
		stats.LogicalBytes = totalUsed.UsedBytes
	}

	// 3. Object-side stats: physical/provider/bucket — all keyed by the set of
	//    object_ids reachable from the (owner-filtered) file table.
	var objectIDs []int64
	if ownerType > 0 && ownerID > 0 {
		ids, err := dal.FindFileObjectIDsByOwner(ctx, s.db, ownerType, ownerID)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by owner")
		}
		objectIDs = ids
	}

	// Physical stats.
	if ownerType > 0 && ownerID > 0 {
		physical, err := dal.CountActiveAndSumObjectSizeByIDs(ctx, s.db, objectIDs)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "physical stats")
		}
		stats.TotalObjects = physical.TotalObjects
		stats.PhysicalBytes = physical.PhysicalBytes
	}

	// Provider stats.
	providerRows, err := dal.GroupObjectsByVendorAndSumSize(ctx, s.db, objectIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "provider stats")
	}
	stats.ProviderStats = make([]providerStat, len(providerRows))
	for i, p := range providerRows {
		stats.ProviderStats[i] = providerStat{
			Vendor:      p.Vendor,
			ObjectCount: p.ObjectCount,
			TotalBytes:  p.TotalBytes,
		}
	}

	// Bucket object stats (also used as base for bucket file stats).
	bucketObjRows, err := dal.GroupObjectsByBucketAndSumSize(ctx, s.db, objectIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "bucket object stats")
	}

	// Bucket file stats: file count per bucket (cross-table in memory).
	bucketFileCount, err := s.computeBucketFileCounts(ctx, ownerType, ownerID, len(bucketObjRows))
	if err != nil {
		return nil, err
	}

	stats.BucketStats = make([]bucketStat, len(bucketObjRows))
	for i, b := range bucketObjRows {
		stats.BucketStats[i] = bucketStat{
			Bucket:      b.Bucket,
			ObjectCount: b.ObjectCount,
			TotalBytes:  b.TotalBytes,
			FileCount:   bucketFileCount[b.Bucket],
		}
	}

	// Owner stats (cross-table in memory, only when no owner filter).
	// Note: TotalObjects/PhysicalBytes above are intentionally only set in the
	// owner-filtered path; global aggregation would require a different query
	// shape (sum over all objects regardless of file references).
	if ownerType <= 0 {
		ownerRows, err := s.computeOwnerStats(ctx)
		if err != nil {
			return nil, err
		}
		stats.OwnerStats = ownerRows
	}

	return stats, nil
}

// computeBucketFileCounts returns a map of bucket -> file count for the given
// owner filter. Composed in memory from FileRepo + dal package functions
// (no cross-table SQL).
func (s *Service) computeBucketFileCounts(ctx context.Context, ownerType int32, ownerID int64, expectedBuckets int) (map[string]int64, error) {
	pairs := make([]models.OwnerObjectIDPair, 0)
	if ownerType > 0 && ownerID > 0 {
		// Filter by owner: read (owner_type, object_id) pairs for this owner.
		ids, err := dal.FindFileObjectIDsByOwner(ctx, s.db, ownerType, ownerID)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "compute bucket file counts")
		}
		for _, id := range ids {
			pairs = append(pairs, models.OwnerObjectIDPair{OwnerType: ownerType, ObjectID: id})
		}
	} else {
		all, err := dal.FindFileOwnerObjectIDPairs(ctx, s.db)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "compute bucket file counts")
		}
		pairs = all
	}

	if len(pairs) == 0 {
		return make(map[string]int64), nil
	}

	// Unique object IDs for batch fetch.
	objIDSet := make(map[int64]struct{}, len(pairs))
	for _, p := range pairs {
		objIDSet[p.ObjectID] = struct{}{}
	}
	uniqueIDs := make([]int64, 0, len(objIDSet))
	for id := range objIDSet {
		uniqueIDs = append(uniqueIDs, id)
	}

	objects, err := dal.BatchGetObjectsByIDs(ctx, s.db, uniqueIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "compute bucket file counts: batch get objects")
	}

	// Count files per bucket (file count = occurrences of object_id in pairs).
	result := make(map[string]int64, expectedBuckets)
	for _, p := range pairs {
		if obj, ok := objects[p.ObjectID]; ok {
			result[obj.Bucket]++
		}
	}
	return result, nil
}

// computeOwnerStats returns per-owner-type aggregate (file count + total bytes).
// Composed in memory from FileRepo.FindOwnerObjectIDPairs + dal.BatchGetObjectsByIDs
// size lookups (no cross-table SQL).
func (s *Service) computeOwnerStats(ctx context.Context) ([]ownerStat, error) {
	pairs, err := dal.FindFileOwnerObjectIDPairs(ctx, s.db)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "compute owner stats")
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	// Unique object IDs for batch fetch.
	objIDSet := make(map[int64]struct{}, len(pairs))
	for _, p := range pairs {
		objIDSet[p.ObjectID] = struct{}{}
	}
	uniqueIDs := make([]int64, 0, len(objIDSet))
	for id := range objIDSet {
		uniqueIDs = append(uniqueIDs, id)
	}

	objects, err := dal.BatchGetObjectsByIDs(ctx, s.db, uniqueIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "compute owner stats: batch get objects")
	}

	// Aggregate by owner_type.
	type agg struct {
		FileCount  int64
		TotalBytes int64
	}
	byOwner := make(map[int32]*agg)
	for _, p := range pairs {
		a, ok := byOwner[p.OwnerType]
		if !ok {
			a = &agg{}
			byOwner[p.OwnerType] = a
		}
		a.FileCount++
		if obj, ok := objects[p.ObjectID]; ok {
			a.TotalBytes += obj.Size
		}
	}

	result := make([]ownerStat, 0, len(byOwner))
	for ownerType, a := range byOwner {
		result = append(result, ownerStat{
			OwnerType:  ownerType,
			FileCount:  a.FileCount,
			TotalBytes: a.TotalBytes,
		})
	}
	return result, nil
}
