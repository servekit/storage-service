// Seed: one-shot importer of legacy YAML providers/buckets/settings into
// the DB platform tables. Run via `storage-service migrate
// --seed-from-config` AFTER the tables exist. Idempotent by name —
// existing rows are skipped. After seeding, remove the providers block
// from YAML: runtime reads the platform tables only.
package handler

import (
	"log/slog"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/xcodes"

	"gorm.io/gorm"
)

// SeedFromConfig imports legacy config-file providers + buckets and the
// default/public bucket settings into the DB platform tables.
func SeedFromConfig(db *gorm.DB, cfg *config.Config) error {
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	importedProviders, importedBuckets := 0, 0

	for _, pc := range cfg.Storage.Providers {
		if pc == nil {
			continue
		}
		vendor, ok := storagev1.Vendor_value[pc.Vendor]
		if !ok {
			slog.Warn("seed: unknown vendor, provider skipped", "provider", pc.Name, "vendor", pc.Vendor)
			continue
		}
		var providerID int64
		existing, err := byName(db, pc.Name)
		if err != nil {
			return err
		}
		if existing != nil {
			providerID = existing.ID
			slog.Info("seed: provider exists, skipped", "provider", pc.Name)
		} else {
			providerID = seedNextID(db)
			row := &models.StorageProvider{
				ID: providerID, Name: pc.Name, Vendor: vendor,
				Endpoint: pc.Endpoint, Region: pc.Region,
				AccessKey: pc.AccessKey, SecretKey: pc.SecretKey,
				RoleARN: pc.RoleARN, DomainID: pc.DomainID,
			}
			if err := db.Create(row).Error; err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
			importedProviders++
		}

		for _, bc := range pc.Buckets {
			if bc == nil {
				continue
			}
			var bucketCount int64
			if err := db.Unscoped().Model(&models.StorageBucket{}).
				Where("name = ?", bc.Name).Count(&bucketCount).Error; err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
			if bucketCount > 0 {
				slog.Info("seed: bucket exists, skipped", "bucket", bc.Name)
				continue
			}
			row := &models.StorageBucket{
				ID: seedNextID(db), Name: bc.Name, ProviderID: providerID,
				ACL: bc.ACL,
			}
			if bc.CDN != nil {
				row.CDNDomain = bc.CDN.Domain
				row.CDNAuthKey = bc.CDN.AuthKey
				row.CDNKeyPairID = bc.CDN.KeyPairID
			}
			if err := db.Create(row).Error; err != nil {
				return xcodes.ErrInternal.Wrap(err)
			}
			importedBuckets++
		}
	}

	// settings: only seed when the row is still empty (never overwrite
	// admin edits).
	settings := &models.StorageSetting{ID: dalSettingsID}
	if err := db.Where("id = ?", dalSettingsID).Take(settings).Error; err != nil {
		if err := db.Create(&models.StorageSetting{
			ID:            dalSettingsID,
			DefaultBucket: cfg.Storage.DefaultBucket,
			PublicBucket:  cfg.Storage.PublicBucket,
		}).Error; err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
	} else if settings.DefaultBucket == "" && settings.PublicBucket == "" {
		settings.DefaultBucket = cfg.Storage.DefaultBucket
		settings.PublicBucket = cfg.Storage.PublicBucket
		if err := db.Save(settings).Error; err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
	}

	slog.Info("seed: platform tables imported (existing names skipped)",
		"providers", importedProviders, "buckets", importedBuckets)

	if ba := cfg.Storage.BootstrapApp; ba != nil && ba.AppKey != "" {
		if err := seedBootstrapApp(db, ba); err != nil {
			return err
		}
	}
	return nil
}

// seedBootstrapApp upserts the deployment's own calling app (e.g. testkit).
// Creation applies the configured key_prefix (must end with '/'); re-runs
// update secret/name/bucket only — the prefix is immutable once objects
// live under it.
func seedBootstrapApp(db *gorm.DB, ba *config.BootstrapAppConfig) error {
	var existing models.StorageApp
	err := db.Where("app_key = ?", ba.AppKey).Take(&existing).Error
	switch {
	case err == nil:
		existing.AppSecret = ba.AppSecret
		existing.Name = ba.Name
		existing.BucketID = ba.BucketID
		if existing.Name == "" {
			existing.Name = ba.AppKey
		}
		if err := db.Save(&existing).Error; err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		slog.Info("seed: bootstrap app updated", "app_key", ba.AppKey)
	case err == gorm.ErrRecordNotFound:
		if ba.KeyPrefix == "" {
			return xcodes.ErrBadRequest.New("bootstrap_app.key_prefix is required on first run (must end with '/')")
		}
		row := &models.StorageApp{
			ID: seedNextID(db), AppKey: ba.AppKey, AppSecret: ba.AppSecret,
			Name: ba.Name, KeyPrefix: ba.KeyPrefix, BucketID: ba.BucketID,
		}
		if row.Name == "" {
			row.Name = ba.AppKey
		}
		if err := db.Create(row).Error; err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		slog.Info("seed: bootstrap app created", "app_key", ba.AppKey, "key_prefix", ba.KeyPrefix)
	default:
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

const dalSettingsID int64 = 1

// byName returns the provider row by name or nil when absent.
func byName(db *gorm.DB, name string) (*models.StorageProvider, error) {
	var row models.StorageProvider
	err := db.Where("name = ?", name).Take(&row).Error
	if err == nil {
		return &row, nil
	}
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return nil, xcodes.ErrInternal.Wrap(err)
}

// seedNextID derives a synthetic ID for the one-shot seed path (max+1) —
// consistent with the storage_platform tables' admin path using gid, but
// the importer must not require a gid connection.
func seedNextID(db *gorm.DB) int64 {
	var maxID int64
	db.Unscoped().Model(&models.StorageProvider{}).Select("COALESCE(MAX(id),0)").Scan(&maxID)
	var maxBucketID int64
	db.Unscoped().Model(&models.StorageBucket{}).Select("COALESCE(MAX(id),0)").Scan(&maxBucketID)
	if maxBucketID > maxID {
		maxID = maxBucketID
	}
	return maxID + 1
}
