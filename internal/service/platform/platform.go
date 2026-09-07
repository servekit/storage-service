// Package platform loads the DB platform tables (providers, buckets,
// settings) and rebuilds the live storage Registry from them. Called at
// startup, after every admin mutation, and by the cron convergence job.
package platform

import (
	"context"
	"fmt"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"

	"gorm.io/gorm"
)

// LoadAndRebuild reads providers + buckets + settings from the DB and
// swaps them into the live Registry. On a Rebuild construction error the
// previous snapshot keeps serving (Rebuild builds into temporaries first).
//
// fallbackDefault/fallbackPublic bootstrap an EMPTY settings row from the
// legacy YAML values (once, persisted) so deployments migrating off config
// keep working until an admin overrides them.
func LoadAndRebuild(ctx context.Context, db *gorm.DB, reg *storage.Registry, fallbackDefault, fallbackPublic string) error {
	providers, err := LoadProviders(ctx, db)
	if err != nil {
		return err
	}
	if err := reg.Rebuild(providers); err != nil {
		return fmt.Errorf("platform: rebuild registry: %w", err)
	}
	settings, err := dal.GetSettings(ctx, db)
	if err != nil {
		return fmt.Errorf("platform: load settings: %w", err)
	}
	if settings.DefaultBucket == "" && settings.PublicBucket == "" &&
		(fallbackDefault != "" || fallbackPublic != "") {
		settings.DefaultBucket = fallbackDefault
		settings.PublicBucket = fallbackPublic
		if err := dal.UpdateSettings(ctx, db, settings); err != nil {
			return fmt.Errorf("platform: bootstrap settings: %w", err)
		}
	}
	reg.SetSettings(settings.DefaultBucket, settings.PublicBucket)
	return nil
}

// LoadProviders converts the DB rows into the config.ProviderConfig shapes
// the Registry consumes.
func LoadProviders(ctx context.Context, db *gorm.DB) ([]*config.ProviderConfig, error) {
	rows, err := dal.ListProviders(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("platform: list providers: %w", err)
	}
	buckets, err := dal.ListBuckets(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("platform: list buckets: %w", err)
	}

	bucketsByProvider := make(map[int64][]*config.BucketConfig)
	for _, b := range buckets {
		bucketsByProvider[b.ProviderID] = append(bucketsByProvider[b.ProviderID], bucketRowToConfig(b))
	}

	out := make([]*config.ProviderConfig, 0, len(rows))
	for _, p := range rows {
		pc := &config.ProviderConfig{
			Name:      p.Name,
			Vendor:    VendorEnumToName(storagev1.Vendor(p.Vendor)),
			Endpoint:  p.Endpoint,
			Region:    p.Region,
			AccessKey: p.AccessKey,
			SecretKey: p.SecretKey,
			RoleARN:   p.RoleARN,
			DomainID:  p.DomainID,
			Disabled:  p.Disabled,
			Buckets:   bucketsByProvider[p.ID],
		}
		out = append(out, pc)
	}
	return out, nil
}

// bucketRowToConfig flattens the CDN columns back into config shapes.
func bucketRowToConfig(b *models.StorageBucket) *config.BucketConfig {
	bc := &config.BucketConfig{
		Name:      b.Name,
		KeyPrefix: b.KeyPrefix,
		ACL:       b.ACL,
	}
	if b.CDNDomain != "" {
		bc.CDN = &config.CDNConfig{
			Domain:    b.CDNDomain,
			AuthKey:   b.CDNAuthKey,
			KeyPairID: b.CDNKeyPairID,
		}
	}
	return bc
}

// BucketRowFromConfig converts an admin payload's bucket shape into a DB
// row (ProviderID resolved by the caller).
func BucketRowFromConfig(name string, providerID int64, keyPrefix, acl string, cdn *storagev1.CDNConfig) *models.StorageBucket {
	b := &models.StorageBucket{
		Name:       name,
		ProviderID: providerID,
		KeyPrefix:  keyPrefix,
		ACL:        acl,
	}
	if cdn != nil {
		b.CDNDomain = cdn.GetDomain()
		b.CDNAuthKey = cdn.GetAuthKey()
		b.CDNKeyPairID = cdn.GetKeyPairId()
	}
	return b
}

// VendorEnumToName renders the proto enum as the config vocabulary string
// ("VENDOR_ALIYUN_OSS"). Unknown values fall back to the number form —
// newProvider rejects them loudly at Rebuild time.
func VendorEnumToName(v storagev1.Vendor) string {
	if name, ok := storagev1.Vendor_name[int32(v)]; ok {
		return name
	}
	return fmt.Sprintf("VENDOR_%d", int32(v))
}
