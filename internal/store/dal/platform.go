// Platform resource data access: providers, buckets, settings + the
// deletion/rebind guards (object/bucket counts). Consumed by the admin
// service and the registry loader.
package dal

import (
	"context"
	"errors"

	"github.com/servekit/storage-service/internal/store/generated"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
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
// key_prefix, acl, cdn).
func UpdateBucket(ctx context.Context, tx *gorm.DB, b *models.StorageBucket) error {
	_, err := gorm.G[models.StorageBucket](tx).
		Where(generated.StorageBucket.ID.Eq(b.ID)).
		Set(
			generated.StorageBucket.ProviderID.Set(b.ProviderID),
			generated.StorageBucket.KeyPrefix.Set(b.KeyPrefix),
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
