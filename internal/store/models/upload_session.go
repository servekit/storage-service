package models

import (
	"time"

	"gorm.io/gorm"
)

// StorageUploadSession represents an in-progress upload: token issued, file not yet
// confirmed. Used for GC (find OSS orphans), audit (record "token issued"),
// and idempotent confirm.
//
// Dedup: idx_upload_sessions_pending_dedup is a non-unique index on
// (owner_type, owner_id, md5, size) used only for FindPendingUploadSessionDedup
// query performance. PENDING-session uniqueness across concurrent callers is
// enforced by a Redis lock in findOrCreateSession — the DB enforces no unique
// constraint here, so the schema is portable across postgres/mysql/sqlite.
// Tradeoff: if Redis is unavailable, two racing callers may both insert a
// PENDING session (duplicates expire via TTL) — accepted for portability.
type StorageUploadSession struct {
	ID          int64   `gorm:"primaryKey;column:id" json:"id"`
	OwnerType   int32   `gorm:"column:owner_type;type:smallint;not null;default:1;index:idx_upload_sessions_owner;index:idx_upload_sessions_pending_dedup,priority:1" json:"owner_type"`
	OwnerID     int64   `gorm:"column:owner_id;not null;index:idx_upload_sessions_owner;index:idx_upload_sessions_pending_dedup,priority:2" json:"owner_id"`
	Bucket      string  `gorm:"column:bucket;type:varchar(128);not null" json:"bucket"`
	ObjectKey   string  `gorm:"column:object_key;type:varchar(512);not null" json:"object_key"`
	MD5         string  `gorm:"column:md5;type:varchar(32);not null;index:idx_upload_sessions_pending_dedup,priority:3" json:"md5"`
	Size        int64   `gorm:"column:size;not null;index:idx_upload_sessions_pending_dedup,priority:4" json:"size"`
	ContentType string  `gorm:"column:content_type;type:varchar(128);not null" json:"content_type"`
	Filename    string  `gorm:"column:filename;type:varchar(256);not null" json:"filename"`
	FilePath    string  `gorm:"column:file_path;type:varchar(512)" json:"file_path,omitempty"`
	Description string  `gorm:"column:description;type:text" json:"description,omitempty"`
	Metadata    MapJSON `gorm:"column:metadata;type:json" json:"metadata,omitempty"`
	IsPublic    bool    `gorm:"column:is_public;not null;default:false" json:"is_public"`
	Vendor      int32   `gorm:"column:vendor;type:smallint;not null" json:"vendor"`

	Status    int32     `gorm:"column:status;type:smallint;not null;default:0;index:idx_upload_sessions_status_expires,priority:1" json:"status"`
	FileID    *int64    `gorm:"column:file_id;index:idx_upload_sessions_file_id" json:"file_id,omitempty"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null;index:idx_upload_sessions_status_expires,priority:2" json:"expires_at"`

	CreatedAt time.Time      `gorm:"column:created_at;not null;autoCreateTime" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null;autoUpdateTime" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at" json:"deleted_at"`
}
