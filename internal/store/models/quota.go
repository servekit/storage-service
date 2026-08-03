package models

import (
	"time"
)

// StorageQuota represents storage quota for an owner. Hard-deleted (no
// DeletedAt) — a stale row would collide with the (owner_type, owner_id) unique
// index on re-create (e.g. admin deletes an owner, then they upload again and
// CreateQuotaIfNotExist runs).
type StorageQuota struct {
	ID         int64     `gorm:"primaryKey" json:"id"`
	OwnerType  int32     `gorm:"column:owner_type;type:smallint;not null;default:1;uniqueIndex:idx_quotas_owner_type_id" json:"owner_type"`
	OwnerID    int64     `gorm:"column:owner_id;not null;uniqueIndex:idx_quotas_owner_type_id" json:"owner_id"`
	TotalBytes int64     `gorm:"column:total_bytes;not null" json:"total_bytes"`
	UsedBytes  int64     `gorm:"column:used_bytes;not null;default:0;check:used_bytes >= 0 AND used_bytes <= total_bytes" json:"used_bytes"`
	CreatedAt  time.Time `gorm:"column:created_at;not null;autoCreateTime" json:"created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null;autoUpdateTime" json:"updated_at"`
}

// --- Single-table aggregation rows on quotas (used by QuotaRepo) ---

// UsedBytesRow holds quota used bytes result (single-table on quotas).
type UsedBytesRow struct {
	UsedBytes int64 `gorm:"column:used_bytes"`
}

// StorageQuotaQuery defines gen-annotated single-table queries on the quotas table.
type StorageQuotaQuery interface {
	// SELECT * FROM @@table WHERE id = @id
	GetActiveByID(id int64) (StorageQuota, error)

	// SELECT COALESCE(used_bytes, 0) AS used_bytes
	// FROM @@table
	// WHERE owner_type = @ownerType AND owner_id = @ownerID
	GetUsedBytes(ownerType int32, ownerID int64) (UsedBytesRow, error)

	// SELECT COALESCE(SUM(used_bytes), 0) AS used_bytes
	// FROM @@table
	GetTotalUsedBytes() (UsedBytesRow, error)
}
