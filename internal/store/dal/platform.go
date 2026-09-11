// Platform resource data access: providers, buckets, settings + the
// deletion/rebind guards (object/bucket counts). Consumed by the admin
// service and the registry loader.
package dal

import (
	"context"
	"errors"
	"time"

	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// --- Providers ---

// CreateProvider inserts a provider row.
func CreateProvider(ctx context.Context, tx *gorm.DB, p *models.StorageProvider) error {
	if err := gorm.G[models.StorageProvider](tx).Create(ctx, p); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetProvider returns the provider by id, or ErrProviderNotFound.
func GetProvider(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageProvider, error) {
	p, err := gorm.G[models.StorageProvider](tx).
		Where(generated.StorageProvider.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrProviderNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &p, nil
}

// GetProviderByName returns the provider by name, or ErrProviderNotFound.
func GetProviderByName(ctx context.Context, tx *gorm.DB, name string) (*models.StorageProvider, error) {
	p, err := gorm.G[models.StorageProvider](tx).
		Where(generated.StorageProvider.Name.Eq(name)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrProviderNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &p, nil
}

// ListProviders returns all providers ordered by name.
func ListProviders(ctx context.Context, tx *gorm.DB) ([]*models.StorageProvider, error) {
	results, err := gorm.G[models.StorageProvider](tx).
		Order(generated.StorageProvider.Name.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*models.StorageProvider, len(results))
	for i := range results {
		out[i] = &results[i]
	}
	return out, nil
}

// UpdateProvider replaces the mutable provider fields (endpoint, region,
// credentials, role_arn, domain_id, disabled). Name and vendor are
// immutable.
func UpdateProvider(ctx context.Context, tx *gorm.DB, p *models.StorageProvider) error {
	_, err := gorm.G[models.StorageProvider](tx).
		Where(generated.StorageProvider.ID.Eq(p.ID)).
		Set(
			generated.StorageProvider.Endpoint.Set(p.Endpoint),
			generated.StorageProvider.Region.Set(p.Region),
			generated.StorageProvider.AccessKey.Set(p.AccessKey),
			generated.StorageProvider.SecretKey.Set(p.SecretKey),
			generated.StorageProvider.RoleARN.Set(p.RoleARN),
			generated.StorageProvider.DomainID.Set(p.DomainID),
			generated.StorageProvider.Disabled.Set(p.Disabled),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeleteProvider soft-deletes the provider. Guarded upstream: providers
// with bound buckets are rejected before this call.
func DeleteProvider(ctx context.Context, tx *gorm.DB, id int64) error {
	_, err := gorm.G[models.StorageProvider](tx).
		Where(generated.StorageProvider.ID.Eq(id)).
		Delete(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// --- Buckets ---

// CreateBucket inserts a bucket row.
func CreateBucket(ctx context.Context, tx *gorm.DB, b *models.StorageBucket) error {
	if err := gorm.G[models.StorageBucket](tx).Create(ctx, b); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetBucket returns the bucket by name, or ErrBucketNotFound.
// GetBucketByID returns the bucket by row id, or ErrBucketNotFound.
func GetBucketByID(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageBucket, error) {
	b, err := gorm.G[models.StorageBucket](tx).
		Where(generated.StorageBucket.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrBucketNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &b, nil
}

func GetBucket(ctx context.Context, tx *gorm.DB, name string) (*models.StorageBucket, error) {
	b, err := gorm.G[models.StorageBucket](tx).
		Where(generated.StorageBucket.Name.Eq(name)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrBucketNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &b, nil
}

// ListBuckets returns all buckets ordered by name.
func ListBuckets(ctx context.Context, tx *gorm.DB) ([]*models.StorageBucket, error) {
	results, err := gorm.G[models.StorageBucket](tx).
		Order(generated.StorageBucket.Name.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*models.StorageBucket, len(results))
	for i := range results {
		out[i] = &results[i]
	}
	return out, nil
}

// UpdateBucket replaces the mutable bucket fields (provider binding,
// acl, cdn).
func UpdateBucket(ctx context.Context, tx *gorm.DB, b *models.StorageBucket) error {
	_, err := gorm.G[models.StorageBucket](tx).
		Where(generated.StorageBucket.ID.Eq(b.ID)).
		Set(
			generated.StorageBucket.ProviderID.Set(b.ProviderID),
			generated.StorageBucket.ACL.Set(b.ACL),
			generated.StorageBucket.CDNDomain.Set(b.CDNDomain),
			generated.StorageBucket.CDNAuthKey.Set(b.CDNAuthKey),
			generated.StorageBucket.CDNKeyPairID.Set(b.CDNKeyPairID),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeleteBucket soft-deletes the bucket. Guarded upstream: buckets with
// existing objects are rejected before this call.
func DeleteBucket(ctx context.Context, tx *gorm.DB, id int64) error {
	_, err := gorm.G[models.StorageBucket](tx).
		Where(generated.StorageBucket.ID.Eq(id)).
		Delete(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// --- Settings ---

// SettingsID is the fixed primary key of the single settings row.
const SettingsID int64 = 1

// GetSettings returns the settings row, creating an empty one on first
// access.
func GetSettings(ctx context.Context, tx *gorm.DB) (*models.StorageSetting, error) {
	s, err := gorm.G[models.StorageSetting](tx).
		Where(generated.StorageSetting.ID.Eq(SettingsID)).
		Take(ctx)
	if err == nil {
		return &s, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	created := &models.StorageSetting{ID: SettingsID}
	if err := gorm.G[models.StorageSetting](tx).Create(ctx, created); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return created, nil
}

// UpdateSettings replaces the settings row values.
func UpdateSettings(ctx context.Context, tx *gorm.DB, s *models.StorageSetting) error {
	_, err := gorm.G[models.StorageSetting](tx).
		Where(generated.StorageSetting.ID.Eq(s.ID)).
		Set(
			generated.StorageSetting.DefaultBucket.Set(s.DefaultBucket),
			generated.StorageSetting.PublicBucket.Set(s.PublicBucket),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// --- guards ---

// CountObjectsInBucket counts storage_object rows in a bucket (deleted
// rows included via Unscoped — soft-deleted objects still reference the
// bucket and would dangle on rebind/delete).
func CountObjectsInBucket(ctx context.Context, tx *gorm.DB, bucket string) (int64, error) {
	var n int64
	err := tx.WithContext(ctx).Unscoped().
		Model(&models.StorageObject{}).
		Where("bucket = ?", bucket).
		Count(&n).Error
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return n, nil
}

// CountBucketsForProvider counts live bucket rows bound to a provider.
func CountBucketsForProvider(ctx context.Context, tx *gorm.DB, providerID int64) (int64, error) {
	var n int64
	err := tx.WithContext(ctx).
		Model(&models.StorageBucket{}).
		Where("provider_id = ?", providerID).
		Count(&n).Error
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return n, nil
}

// --- Apps ---

// CreateApp inserts an app row.
func CreateApp(ctx context.Context, tx *gorm.DB, a *models.StorageApp) error {
	if err := gorm.G[models.StorageApp](tx).Create(ctx, a); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// EnsureTenantApp idempotently inserts the first-sight tenant config row
// (trusted x-tenant-key path). The conflict target is intentionally left
// broad (ON CONFLICT DO NOTHING, no column list): the lazy row can collide
// on app_key OR on key_prefix ("{tenant_key}/" — an operator may have
// pre-created it), and racing replicas must converge on one row either way.
// The caller re-reads after the insert, so DoNothing never clobbers
// operator edits.
//
// F2 hardening: a SOFT-DELETED occupant holding the tenant's unique keys
// makes the insert silently no-op while the scoped re-read misses it — the
// one-off INTERNAL "row absent after insert" (T11 F2 anomaly; the
// T6-recorded soft-delete-occupant edge). Ensure semantics now demand more
// than a suppressed insert: the occupant is looked up UNSCOPED (by
// tenant_key, then the app_key-equal fallback mirroring GetAppForTenant)
// and revived in place — deleted_at cleared, tenant_key re-pointed — while
// its historic identity fields stay verbatim (key_prefix is immutable,
// objects may live under it; the secret/name are the operator's row). The
// lookup is ordered live-first (③-review F2 corner): when a LIVE row and a
// soft-deleted one both match the OR, the live one wins and the dead one
// stays dead instead of being revived into a second claimant of the
// tenant's unique keys.
func EnsureTenantApp(ctx context.Context, tx *gorm.DB, record *models.StorageApp) error {
	if err := gorm.G[models.StorageApp](tx, clause.OnConflict{
		DoNothing: true,
	}).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}

	tk := models.TenantKeyOf(record.TenantKey)
	var occupant models.StorageApp
	err := tx.WithContext(ctx).Unscoped().
		Order(clause.Expr{SQL: "deleted_at IS NULL DESC"}).
		Order("id").
		Where("tenant_key = ? OR app_key = ?", tk, record.AppKey).
		Take(&occupant).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// No occupant on either unique key: the insert landed (the
			// caller's re-read confirms) or the blocker is a key_prefix-only
			// row mapped elsewhere — an operator decision, left to the
			// caller's actionable error.
			return nil
		}
		return xcodes.ErrInternal.Wrap(err)
	}
	if occupant.DeletedAt.Time.IsZero() && !occupant.DeletedAt.Valid {
		return nil // live occupant: race winner or existing row — never clobbered
	}
	res := tx.WithContext(ctx).Unscoped().
		Model(&models.StorageApp{}).
		Where("id = ?", occupant.ID).
		Updates(map[string]any{
			"deleted_at": nil,
			"tenant_key": tk,
			"updated_at": time.Now(),
		})
	if res.Error != nil {
		return xcodes.ErrInternal.Wrap(res.Error)
	}
	return nil
}

// GetAppForTenant resolves the tenant's config row: prefer the tenant_key
// mapping, fall back to app_key = tenantKey (pre-backfill window rows whose
// column is still NULL). nil when neither matches.
func GetAppForTenant(ctx context.Context, tx *gorm.DB, tenantKey string) (*models.StorageApp, error) {
	record, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.TenantKey.Eq(tenantKey)).
		Take(ctx)
	if err == nil {
		return &record, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	record, err = gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.AppKey.Eq(tenantKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// CountAppsByTenantKey guards the admin create path: tenant_key uniqueness
// over live rows (the DB unique index is the last line of defense; this
// gives the operator a friendly error instead of an internal one).
func CountAppsByTenantKey(ctx context.Context, tx *gorm.DB, tenantKey string) (int64, error) {
	n, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.TenantKey.Eq(tenantKey)).
		Count(ctx, "*")
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return n, nil
}

// GetApp returns the app by id, or ErrAppNotFound.
func GetApp(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageApp, error) {
	a, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrAppNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &a, nil
}

// GetAppByKey returns the live app by app_key, or ErrAppNotFound.
func GetAppByKey(ctx context.Context, tx *gorm.DB, appKey string) (*models.StorageApp, error) {
	a, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.AppKey.Eq(appKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrAppNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &a, nil
}

// ListApps returns all live apps ordered by app_key.
func ListApps(ctx context.Context, tx *gorm.DB) ([]*models.StorageApp, error) {
	results, err := gorm.G[models.StorageApp](tx).
		Order(generated.StorageApp.AppKey.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*models.StorageApp, len(results))
	for i := range results {
		out[i] = &results[i]
	}
	return out, nil
}

// UpdateApp applies mutable fields (name / disabled / bucket binding);
// app_key and key_prefix are immutable by design.
func UpdateApp(ctx context.Context, tx *gorm.DB, a *models.StorageApp) error {
	_, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.ID.Eq(a.ID)).
		Set(
			generated.StorageApp.Name.Set(a.Name),
			generated.StorageApp.Disabled.Set(a.Disabled),
			generated.StorageApp.BucketID.Set(a.BucketID),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeleteApp soft-deletes the app. Data-plane calls fail immediately via the
// registry snapshot; existing objects/files stay readable.
func DeleteApp(ctx context.Context, tx *gorm.DB, id int64) error {
	_, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.ID.Eq(id)).
		Delete(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// CountAppsByBucketID guards bucket deletion: a bucket referenced by an
// app's binding cannot be removed.
func CountAppsByBucketID(ctx context.Context, tx *gorm.DB, bucketID int64) (int64, error) {
	n, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.BucketID.Eq(bucketID)).
		Count(ctx, "*")
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return n, nil
}

// CountAppsByKeyPrefix guards prefix uniqueness over live rows (soft-deleted
// rows may reuse the prefix — re-creating a deleted app reactivates it).
func CountAppsByKeyPrefix(ctx context.Context, tx *gorm.DB, keyPrefix string) (int64, error) {
	n, err := gorm.G[models.StorageApp](tx).
		Where(generated.StorageApp.KeyPrefix.Eq(keyPrefix)).
		Count(ctx, "*")
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return n, nil
}
