package admin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/servekit/storage-service/internal/store/dal"

	"gorm.io/gorm"
)

// PurgeDeletedObjects soft-deletes zero-ref objects, then hard-deletes expired
// soft-deleted objects along with their cloud storage files.
// Flow: query DB -> delete cloud -> hard-delete DB, per object in a transaction.
func (s *Service) PurgeDeletedObjects(ctx context.Context) (int, error) {
	// Step 1: Soft-delete active objects whose ref count dropped to 0.
	zeroRefCount, err := dal.DeleteZeroRefCountObjects(ctx, s.db)
	if err != nil {
		return 0, fmt.Errorf("delete zero ref count: %w", err)
	}
	if zeroRefCount > 0 {
		slog.Info("soft-deleted zero ref count objects", "count", zeroRefCount)
	}

	// Step 2: Find purgeable objects from DB (soft-deleted, past retention, ref_count=0).
	cutoff := time.Now().Add(-s.cfg.Storage.SoftDeleteRetention)
	objects, err := dal.FindPurgeableObjects(ctx, s.db, cutoff)
	if err != nil {
		return 0, fmt.Errorf("find purgeable: %w", err)
	}
	if len(objects) == 0 {
		return 0, nil
	}

	// Step 3: Delete cloud files first, then hard-delete DB records in transactions.
	deleted := 0
	for i := range objects {
		obj := objects[i]

		p, provErr := s.registry.ProviderForBucket(obj.Bucket)
		if provErr != nil {
			slog.Error("failed to get provider for purge", "bucket", obj.Bucket, "object_id", obj.ID, "error", provErr)
			continue
		}
		if delErr := p.DeleteObject(ctx, obj.Bucket, obj.ObjectKey); delErr != nil {
			slog.Error("failed to delete object from cloud", "bucket", obj.Bucket, "key", obj.ObjectKey, "error", delErr)
			continue
		}

		if delErr := dal.PurgeObject(ctx, s.db, obj.ID); delErr != nil {
			slog.Error("failed to purge object", "id", obj.ID, "error", delErr)
			continue
		}
		deleted++
	}
	return deleted, nil
}

// PurgeDeletedOwner removes all files and decrements object ref counts for owners
// soft-deleted beyond the retention period, all within a single transaction.
func (s *Service) PurgeDeletedOwner(ctx context.Context, ownerType int32, ownerID int64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. Get ref counts BEFORE deleting files (GORM auto-filters deleted_at IS NULL).
		refCounts, err := dal.GetFileObjectRefCountsByOwner(ctx, tx, ownerType, ownerID)
		if err != nil {
			return fmt.Errorf("get ref counts for owner %d/%d: %w", ownerType, ownerID, err)
		}

		// 2. Soft-delete all active files for the owner.
		fileCount, err := dal.DeleteFilesByOwner(ctx, tx, ownerType, ownerID)
		if err != nil {
			return fmt.Errorf("delete files for owner %d/%d: %w", ownerType, ownerID, err)
		}

		// 3. Decrement object ref counts.
		for objectID, count := range refCounts {
			if decrErr := dal.DecrObjectRefCountBy(ctx, tx, objectID, count); decrErr != nil {
				return fmt.Errorf("decr ref count for object %d: %w", objectID, decrErr)
			}
		}

		// 4. Soft-delete quota.
		if err := dal.DeleteQuotaByOwner(ctx, tx, ownerType, ownerID); err != nil {
			return fmt.Errorf("delete quota for owner %d/%d: %w", ownerType, ownerID, err)
		}

		slog.Info("purged deleted owner", "owner_type", ownerType, "owner_id", ownerID, "files", fileCount)
		return nil
	})
}

// DeletedOwnerRetention returns the configured retention duration for purging deleted owners.
func (s *Service) DeletedOwnerRetention() time.Duration {
	return s.cfg.Storage.DeletedOwnerRetention
}
