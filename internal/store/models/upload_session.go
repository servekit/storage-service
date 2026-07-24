package models

import (
	"time"

	"gorm.io/gorm"
)

// StorageUploadSession represents an in-progress upload: token issued, file not yet
// confirmed. Used for GC (find OSS orphans), audit (record "token issued"),
// and idempotent confirm.
//
// Dedup: idx_upload_sessions_pending_dedup is a partial unique index on
// (owner_type, owner_id, md5, size) scoped to status=PENDING and not-deleted.
// It is the DB-level backstop for findOrCreateSession: even if the Redis dedup
// lock is unavailable, two concurrent callers cannot both insert a PENDING
// session for the same key — the loser's INSERT hits ON CONFLICT DO NOTHING
// (see UploadSessionRepo.Create) and the caller re-reads via FindPendingDedup.
// CONFIRMED/EXPIRED/CANCELLED rows are out of the predicate, so subsequent
// uploads of the same key can create a fresh PENDING session.
type StorageUploadSession struct {
	ID          int64   `gorm:"primaryKey;column:id" json:"id"`
	OwnerType   int32   `gorm:"column:owner_type;type:smallint;not null;default:1;index:idx_upload_sessions_owner,condition:deleted_at IS NULL;uniqueIndex:idx_upload_sessions_pending_dedup,priority:1,condition:status = 1 AND deleted_at IS NULL" json:"owner_type"`
	OwnerID     int64   `gorm:"column:owner_id;not null;index:idx_upload_sessions_owner,condition:deleted_at IS NULL;uniqueIndex:idx_upload_sessions_pending_dedup,priority:2,condition:status = 1 AND deleted_at IS NULL" json:"owner_id"`
	Bucket      string  `gorm:"column:bucket;type:varchar(128);not null" json:"bucket"`
	ObjectKey   string  `gorm:"column:object_key;type:varchar(512);not null" json:"object_key"`
	MD5         string  `gorm:"column:md5;type:varchar(32);not null;uniqueIndex:idx_upload_sessions_pending_dedup,priority:3,condition:status = 1 AND deleted_at IS NULL" json:"md5"`
	Size        int64   `gorm:"column:size;not null;uniqueIndex:idx_upload_sessions_pending_dedup,priority:4,condition:status = 1 AND deleted_at IS NULL" json:"size"`
	ContentType string  `gorm:"column:content_type;type:varchar(128);not null" json:"content_type"`
	Filename    string  `gorm:"column:filename;type:varchar(256);not null" json:"filename"`
	FilePath    string  `gorm:"column:file_path;type:varchar(512)" json:"file_path,omitempty"`
	Description string  `gorm:"column:description;type:text" json:"description,omitempty"`
	Metadata    MapJSON `gorm:"column:metadata;type:jsonb" json:"metadata,omitempty"`
	IsPublic    bool    `gorm:"column:is_public;not null;default:false" json:"is_public"`
	Vendor      int32   `gorm:"column:vendor;type:smallint;not null" json:"vendor"`

	Status    int32     `gorm:"column:status;type:smallint;not null;default:0;index:idx_upload_sessions_status_expires,priority:1,condition:deleted_at IS NULL" json:"status"`
	FileID    *int64    `gorm:"column:file_id;index:idx_upload_sessions_file_id" json:"file_id,omitempty"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null;index:idx_upload_sessions_status_expires,priority:2,condition:deleted_at IS NULL" json:"expires_at"`

	CreatedAt time.Time      `gorm:"column:created_at;not null;autoCreateTime" json:"created_at"`
	UpdatedAt time.Time      `gorm:"column:updated_at;not null;autoUpdateTime" json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"column:deleted_at" json:"deleted_at"`
}
