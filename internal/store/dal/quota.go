package dal

import (
	"context"
	"errors"

	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GetQuotaByOwner retrieves the quota for an owner.
func GetQuotaByOwner(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64) (*models.StorageQuota, error) {
	q, err := gorm.G[models.StorageQuota](tx).
		Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
		Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrQuotaNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &q, nil
}

// CreateQuotaIfNotExist creates a quota record if one does not already exist.
// id is the pre-generated snowflake ID for the new record.
// Returns the existing or newly created quota.
func CreateQuotaIfNotExist(ctx context.Context, tx *gorm.DB, id int64, ownerType int32, ownerID, totalBytes int64) (*models.StorageQuota, error) {
	q := &models.StorageQuota{
		ID:         id,
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TotalBytes: totalBytes,
	}

	result := tx.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				generated.StorageQuota.OwnerType.Column(),
				generated.StorageQuota.OwnerID.Column(),
			},
			DoNothing: true,
		}).
		Create(q)

	if result.Error != nil {
		return nil, xcodes.ErrInternal.Wrapf(result.Error, "create if not exist")
	}

	if result.RowsAffected > 0 {
		return q, nil
	}

	existing, err := GetQuotaByOwner(ctx, tx, ownerType, ownerID)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "get existing quota")
	}
	return existing, nil
}

// IncrementQuotaUsed atomically increases the used bytes for an owner.
// Fails if the increment would exceed the total quota.
//
// Cross-column arithmetic (`used_bytes + N <= total_bytes`) cannot be expressed
// by gorm.io/cli's typed field helpers, and the gorm.io/cli v0.2.4 UPDATE-template
// path cannot return RowsAffected (only single-result `error` or two-result
// `(T, error)` via Scan() — the latter is SELECT semantics and yields 0 for
// UPDATE, breaking rowsAffected==0 → ErrQuotaExceeded mapping). Raw SQL
// `Where("used_bytes + ? <= total_bytes", bytes)` is the parameter-bound
// workaround; the typed builder's `Set(...)` and `Update(ctx)` still produce
// the atomic UPDATE and surface rowsAffected.
//
// rowsAffected == 0 means the row exists but the condition failed → quota
// exceeded. Works correctly inside outer transactions because a single UPDATE
// is atomic per-statement regardless of caller's transaction context.
func IncrementQuotaUsed(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, bytes int64) error {
	rowsAffected, err := gorm.G[models.StorageQuota](tx).
		Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
		Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
		// Raw SQL cross-column arithmetic condition required: generated field
		// helpers cannot express `used_bytes + N <= total_bytes`. The `?` is
		// parameter-bound (no injection risk).
		Where("used_bytes + ? <= total_bytes", bytes).
		Set(generated.StorageQuota.UsedBytes.Incr(bytes)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "increment used")
	}
	if rowsAffected == 0 {
		return xcodes.ErrQuotaExceeded.New()
	}
	return nil
}

// DecrementQuotaUsed atomically decreases the used bytes for an owner.
func DecrementQuotaUsed(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, bytes int64) error {
	rowsAffected, err := gorm.G[models.StorageQuota](tx).
		Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
		Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
		Where(generated.StorageQuota.UsedBytes.Gte(bytes)).
		Set(generated.StorageQuota.UsedBytes.Decr(bytes)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "decrement used")
	}
	if rowsAffected == 0 {
		return xcodes.ErrQuotaInsufficientUsed.New()
	}
	return nil
}

// SetQuota updates the total quota for an owner.
func SetQuota(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, totalBytes int64) error {
	rowsAffected, err := gorm.G[models.StorageQuota](tx).
		Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
		Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
		Set(generated.StorageQuota.TotalBytes.Set(totalBytes)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "set quota")
	}
	if rowsAffected == 0 {
		return xcodes.ErrQuotaNotActive.New()
	}
	return nil
}

// AddQuota atomically increments the owner's total quota by delta (may be negative
// for refund). The WHERE clause rejects the update if the result would go negative,
// so a rowsAffected == 0 result means either no quota row exists or the refund
// would push total below zero — service layer pre-creates the row via ensureQuota,
// so in practice this means "refund too large".
//
// Cross-column arithmetic (`total_bytes + N >= 0`) cannot be expressed by gorm.io/cli's
// typed field helpers, and the gorm.io/cli v0.2.4 UPDATE-template path
// cannot return RowsAffected (see IncrementQuotaUsed doc). Raw SQL
// `Where("total_bytes + ? >= 0", delta)` is the parameter-bound workaround.
//
// Caller must wrap in a transaction to guarantee atomicity with other operations.
// Works correctly inside outer transactions (single atomic UPDATE).
func AddQuota(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, delta int64) error {
	rowsAffected, err := gorm.G[models.StorageQuota](tx).
		Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
		Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
		Where("total_bytes + ? >= 0", delta).
		Set(generated.StorageQuota.TotalBytes.Incr(delta)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "add quota")
	}
	if rowsAffected == 0 {
		return xcodes.ErrQuotaInsufficientTotal.New()
	}
	return nil
}

// DeleteQuotaByOwner hard-deletes the quota row for an owner (StorageQuota has
// no DeletedAt, so this is a physical DELETE).
func DeleteQuotaByOwner(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID int64) error {
	_, err := gorm.G[models.StorageQuota](tx).
		Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
		Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
		Delete(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "delete quota")
	}
	return nil
}
