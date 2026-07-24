package models

import (
	"time"

	"gorm.io/gorm"
)

// StorageQuota represents storage quota for an owner.
type StorageQuota struct {
	ID         int64          `gorm:"primaryKey" json:"id"`
	OwnerType  int32          `gorm:"column:owner_type;type:smallint;not null;default:1;uniqueIndex:idx_quotas_owner_type_id" json:"owner_type"`
	OwnerID    int64          `gorm:"column:owner_id;not null;uniqueIndex:idx_quotas_owner_type_id" json:"owner_id"`
	TotalBytes int64          `gorm:"column:total_bytes;not null" json:"total_bytes"`
	UsedBytes  int64          `gorm:"column:used_bytes;not null;default:0;check:used_bytes >= 0 AND used_bytes <= total_bytes" json:"used_bytes"`
	DeletedAt  gorm.DeletedAt `gorm:"column:deleted_at" json:"deleted_at"`
	CreatedAt  time.Time      `gorm:"column:created_at;not null;autoCreateTime" json:"created_at"`
	UpdatedAt  time.Time      `gorm:"column:updated_at;not null;autoUpdateTime" json:"updated_at"`
}

// --- Single-table aggregation rows on quotas (used by QuotaRepo) ---

// UsedBytesRow holds quota used bytes result (single-table on quotas).
type UsedBytesRow struct {
	UsedBytes int64 `gorm:"column:used_bytes"`
}

// StorageQuotaQuery defines gen-annotated single-table queries on the quotas table.
type StorageQuotaQuery interface {
	// SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
	GetActiveByID(id int64) (StorageQuota, error)

	// SELECT COALESCE(used_bytes, 0) AS used_bytes
	// FROM @@table
	// WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
	GetUsedBytes(ownerType int32, ownerID int64) (UsedBytesRow, error)

	// SELECT COALESCE(SUM(used_bytes), 0) AS used_bytes
	// FROM @@table
	// WHERE deleted_at IS NULL
	GetTotalUsedBytes() (UsedBytesRow, error)
}
