package models

import (
	"time"

	"gorm.io/gorm"
)

// StorageObject represents a physical file in cloud storage, deduplicated by
// (vendor, bucket, md5).
type StorageObject struct {
	ID           int64  `gorm:"primaryKey" json:"id"`
	Vendor       int32  `gorm:"column:vendor;type:smallint;not null;index:idx_storage_objects_vendor_bucket_md5" json:"vendor"`
	Bucket       string `gorm:"column:bucket;type:varchar(128);not null;index:idx_storage_objects_vendor_bucket_md5;index:idx_storage_objects_bucket_key_active" json:"bucket"`
	ObjectKey    string `gorm:"column:object_key;type:varchar(512);not null;index:idx_storage_objects_bucket_key_active" json:"object_key"`
	MD5          string `gorm:"column:md5;type:varchar(32);not null;index:idx_storage_objects_vendor_bucket_md5" json:"md5"`
	Size         int64  `gorm:"column:size;not null" json:"size"`
	ContentType  string `gorm:"column:content_type;type:varchar(128);not null" json:"content_type"`
	Extension    string `gorm:"column:extension;type:varchar(16)" json:"extension,omitempty"`
	ETag         string `gorm:"column:etag;type:varchar(128)" json:"etag,omitempty"`
	StorageClass int32  `gorm:"column:storage_class;type:smallint;not null;default:1" json:"storage_class"`
	// IsPublic is derived from BucketConfig.ACL == "public_read" at upload time.
	// It is the source of truth for whether the file's bucket is publicly
	// readable; StorageFile.IsPublic mirrors it for query convenience.
	IsPublic  bool           `gorm:"column:is_public;not null;default:false" json:"is_public"`
	RefCount  int64          `gorm:"column:ref_count;not null;default:0;check:ref_count >= 0" json:"ref_count"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at" json:"deleted_at"`
	CreatedAt time.Time      `gorm:"column:created_at;not null;autoCreateTime" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null;autoUpdateTime" json:"updated_at"`
}

// --- Single-table aggregation rows on storage_objects (used by ObjectRepo) ---

// PhysicalStatsRow holds aggregate physical storage statistics (single-table
// on storage_objects).
type PhysicalStatsRow struct {
	TotalObjects  int64 `gorm:"column:total_objects"`
	PhysicalBytes int64 `gorm:"column:physical_bytes"`
}

// ProviderStatRow holds per-provider aggregate statistics (single-table
// aggregation on storage_objects, grouped by vendor).
type ProviderStatRow struct {
	Vendor      int32 `gorm:"column:vendor"`
	ObjectCount int64 `gorm:"column:object_count"`
	TotalBytes  int64 `gorm:"column:total_bytes"`
}

// BucketObjectStatRow holds per-bucket object aggregate statistics
// (single-table aggregation on storage_objects, grouped by bucket).
type BucketObjectStatRow struct {
	Bucket      string `gorm:"column:bucket"`
	ObjectCount int64  `gorm:"column:object_count"`
	TotalBytes  int64  `gorm:"column:total_bytes"`
}

// StorageObjectQuery defines gen-annotated single-table queries on storage_objects.
type StorageObjectQuery interface {
	// SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
	GetActiveByID(id int64) (StorageObject, error)

	// SELECT COUNT(*) AS total_objects,
	//        COALESCE(SUM(size), 0) AS physical_bytes
	// FROM @@table
	// WHERE id IN (@ids) AND deleted_at IS NULL
	CountActiveAndSumSize(ids []int64) (PhysicalStatsRow, error)

	// SELECT vendor,
	//        COUNT(*) AS object_count,
	//        COALESCE(SUM(size), 0) AS total_bytes
	// FROM @@table
	// WHERE id IN (@ids) AND deleted_at IS NULL
	// GROUP BY vendor
	GroupByVendorCountAndSumSize(ids []int64) ([]ProviderStatRow, error)

	// SELECT bucket,
	//        COUNT(*) AS object_count,
	//        COALESCE(SUM(size), 0) AS total_bytes
	// FROM @@table
	// WHERE id IN (@ids) AND deleted_at IS NULL
	// GROUP BY bucket
	GroupByBucketCountAndSumSize(ids []int64) ([]BucketObjectStatRow, error)
}
