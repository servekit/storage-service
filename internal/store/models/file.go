package models

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/servekit/go-common/jsonx"
	"gorm.io/gorm"
)

// StorageFile represents a file mapping to a physical storage object.
type StorageFile struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	OwnerType   int32          `gorm:"column:owner_type;type:smallint;not null;default:1;index:idx_files_owner" json:"owner_type"`
	OwnerID     int64          `gorm:"column:owner_id;not null;index:idx_files_owner" json:"owner_id"`
	ObjectID    int64          `gorm:"column:object_id;not null;index:idx_files_object_id" json:"object_id"`
	Filename    string         `gorm:"column:filename;type:varchar(256);not null" json:"filename"`
	FilePath    string         `gorm:"column:file_path;type:varchar(512)" json:"file_path,omitempty"`
	Description string         `gorm:"column:description;type:text" json:"description,omitempty"`
	Metadata    MapJSON        `gorm:"column:metadata;type:json" json:"metadata,omitempty"`
	IsPublic    bool           `gorm:"column:is_public;not null;default:false" json:"is_public"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at;index:idx_files_owner;index:idx_files_owner_path" json:"deleted_at"`
	CreatedAt   time.Time      `gorm:"column:created_at;not null;autoCreateTime" json:"created_at"`
	UpdatedAt   time.Time      `gorm:"column:updated_at;not null;autoUpdateTime" json:"updated_at"`
}

// MapJSON is a custom type for JSONB map fields.
type MapJSON map[string]string

// --- Single-table aggregation rows (used by FileRepo + service layer) ---

// FileCountRow holds the result of a file count query (single-table on files).
type FileCountRow struct {
	Count int64 `gorm:"column:count"`
}

// ObjectRefCountRow holds the result of file count aggregation grouped by
// object_id (single-table on files).
type ObjectRefCountRow struct {
	ObjectID int64 `gorm:"column:object_id"`
	Count    int64 `gorm:"column:count"`
}

// OwnerObjectIDPair holds a (owner_type, object_id) pair from active files.
// Used by service layer to compute OwnerStats by composing FileRepo results
// with ObjectRepo size lookups.
type OwnerObjectIDPair struct {
	OwnerType int32 `gorm:"column:owner_type"`
	ObjectID  int64 `gorm:"column:object_id"`
}

// StorageFileQuery defines gen-annotated single-table queries on the files table.
// gorm gen processes these annotations and generates type-safe implementations
// in internal/store/generated.
type StorageFileQuery interface {
	// SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
	GetActiveByID(id int64) (StorageFile, error)

	// SELECT object_id, COUNT(*) AS count
	// FROM @@table
	// WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
	// GROUP BY object_id
	GetObjectRefCounts(ownerType int32, ownerID int64) ([]ObjectRefCountRow, error)

	// SELECT COUNT(*) AS count
	// FROM @@table
	// {{where}}
	//   deleted_at IS NULL
	//   {{if ownerType > 0}} AND owner_type = @ownerType {{end}}
	//   {{if ownerID > 0}} AND owner_id = @ownerID {{end}}
	// {{end}}
	GetFileCount(ownerType int32, ownerID int64) (FileCountRow, error)

	// SELECT owner_type, object_id
	// FROM @@table
	// WHERE deleted_at IS NULL
	// ORDER BY id
	// LIMIT @limit
	FindOwnerObjectIDPairs(limit int) ([]OwnerObjectIDPair, error)
}

// Value implements the driver.Valuer interface.
func (m MapJSON) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return jsonx.Marshal(m)
}

// Scan implements the sql.Scanner interface. Handles both []byte
// (postgres/mysql) and string (sqlite/modernc) for cross-dialect portability.
func (m *MapJSON) Scan(value any) error {
	if value == nil {
		*m = nil
		return nil
	}
	var bytes []byte
	switch v := value.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return fmt.Errorf("cannot scan %T into MapJSON", value)
	}
	return jsonx.Unmarshal(bytes, m)
}
