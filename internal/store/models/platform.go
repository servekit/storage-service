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
type StorageBucket struct {
	ID         int64  `gorm:"primaryKey"`
	Name       string `gorm:"size:255;uniqueIndex;not null"`
	ProviderID int64  `gorm:"column:provider_id;index;not null"`
	KeyPrefix  string `gorm:"column:key_prefix;size:256"`
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

// StorageSetting is the single runtime settings row (ID = 1): default and
// public bucket names, moved out of YAML for admin-surface management.
type StorageSetting struct {
	ID            int64  `gorm:"primaryKey"`
	DefaultBucket string `gorm:"column:default_bucket;size:255"`
	PublicBucket  string `gorm:"column:public_bucket;size:255"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
