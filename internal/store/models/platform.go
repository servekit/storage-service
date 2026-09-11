// Platform resource models: the provider/bucket/settings registry rows
// backing the hot-reloadable storage Registry. Managed via the admin RPCs
// (AdminCreateProvider / AdminUpsertBucket / AdminUpdateSettings); loaded
// and built into live vendor clients on every Registry refresh.
package models

import (
	"time"

	"gorm.io/gorm"
)

// StorageProvider is one vendor credential set. Vendor is the proto
// storage.v1.Vendor enum value; credentials are stored PLAINTEXT
// (internal-trust posture, mirroring message-service channel accounts).
type StorageProvider struct {
	ID        int64  `gorm:"primaryKey"`
	Name      string `gorm:"size:64;uniqueIndex;not null"`
	Vendor    int32  `gorm:"not null"`
	Endpoint  string `gorm:"size:512"`
	Region    string `gorm:"size:64"`
	AccessKey string `gorm:"size:256;not null"`
	SecretKey string `gorm:"size:256;not null"`
	// RoleARN enables STS for this provider; empty = STS unavailable.
	RoleARN string `gorm:"column:role_arn;size:256"`
	// DomainID is the Huawei Cloud account UID (VENDOR_HUAWEI_OBS only).
	DomainID string `gorm:"column:domain_id;size:64"`
	// Disabled providers stay readable (existing objects) but new uploads
	// targeting their buckets are rejected.
	Disabled  bool `gorm:"not null;default:false"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

// StorageBucket binds a bucket to a provider. CDN columns are the flattened
// CDNConfig; empty cdn_domain = CDN disabled for the bucket.
//
// Key prefixes no longer live here — they belong to apps
// (StorageApp.KeyPrefix). The legacy key_prefix column is left dormant in
// the database (AutoMigrate does not drop columns); existing objects keep
// their stored object keys and stay readable.
type StorageBucket struct {
	ID         int64  `gorm:"primaryKey"`
	Name       string `gorm:"size:255;uniqueIndex;not null"`
	ProviderID int64  `gorm:"column:provider_id;index;not null"`
	// ACL is the config-string form ("private" | "public-read") — the same
	// vocabulary the upload path already matches on.
	ACL string `gorm:"size:32;not null;default:private"`
	// CDN fronting config (flattened CDNConfig).
	CDNDomain    string `gorm:"column:cdn_domain;size:255"`
	CDNAuthKey   string `gorm:"column:cdn_auth_key;size:256"`
	CDNKeyPairID string `gorm:"column:cdn_key_pair_id;size:128"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	DeletedAt    gorm.DeletedAt `gorm:"index"`
}

// StorageApp is one calling application of the storage platform. Data-plane
// RPCs authenticate with (app_key, app_secret) metadata; every object the app
// writes lives under its globally-unique key_prefix, which is also the
// dedup domain. Mirrors message-service's MessageApp pattern (secret stored
// PLAINTEXT — internal-trust posture).
type StorageApp struct {
	ID        int64  `gorm:"primaryKey"`
	AppKey    string `gorm:"column:app_key;size:64;uniqueIndex;not null"`
	AppSecret string `gorm:"column:app_secret;size:128;not null"`
	Name      string `gorm:"size:200;not null"`
	// KeyPrefix namespaces every object the app writes; immutable, ends '/'.
	// Phase ③: EXISTING apps keep their prefix verbatim (never recomputed —
	// objects already live under it); tenants first seen on the trusted path
	// are lazily created with "{tenant_key}/".
	KeyPrefix string `gorm:"column:key_prefix;size:64;uniqueIndex;not null"`
	// TenantKey maps the app to its tenant. Nullable transition: NULL = not
	// yet backfilled; the data plane falls back to the app_key literal (T10
	// 总装 clears the empties). Unique —
	// one config row per tenant.
	TenantKey *string `gorm:"size:16;column:tenant_key;uniqueIndex:uniq_storage_apps_tenant_key"`
	// BucketID selects the app's private bucket; 0 = the default bucket.
	BucketID int64 `gorm:"column:bucket_id;not null;default:0"`
	// Disabled apps fail every data-plane call immediately.
	Disabled  bool           `gorm:"not null;default:false"`
	CreatedAt time.Time      `gorm:"column:created_at;not null;autoCreateTime"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null;autoUpdateTime"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

// StorageSetting is the single runtime settings row (ID = 1): default and
// public bucket names, moved out of YAML for admin-surface management.
type StorageSetting struct {
	ID            int64  `gorm:"primaryKey"`
	DefaultBucket string `gorm:"column:default_bucket;size:255"`
	PublicBucket  string `gorm:"column:public_bucket;size:255"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// --- tenant_key helpers (nullable-column ergonomics, phase ③ window) ---

// TenantKeyOf dereferences a nullable tenant_key column; nil → "".
func TenantKeyOf(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// TenantKeyPtr boxes a tenant key; "" → nil (writes NULL — the
// not-yet-backfilled marker).
func TenantKeyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AppTenantKey resolves the tenant an app row maps to: the tenant_key column
// when backfilled, else the app_key literal (phase ③ window fallback —
// T10 总装 clears the empty columns).
func AppTenantKey(a *StorageApp) string {
	if a == nil {
		return ""
	}
	if tk := TenantKeyOf(a.TenantKey); tk != "" {
		return tk
	}
	return a.AppKey
}
