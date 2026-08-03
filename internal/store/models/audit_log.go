package models

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/servekit/go-common/jsonx"
)

// StorageAuditLog records a write operation on a storage resource.
type StorageAuditLog struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	Action       int32     `gorm:"column:action;type:smallint;not null" json:"action"`
	OwnerType    int32     `gorm:"column:owner_type;type:smallint;not null;default:1;index:idx_audit_logs_owner,sort:desc" json:"owner_type"`
	OwnerID      int64     `gorm:"column:owner_id;not null;index:idx_audit_logs_owner,sort:desc" json:"owner_id"`
	TargetType   int32     `gorm:"column:target_type;type:smallint;not null;index:idx_audit_logs_target,sort:desc" json:"target_type"`
	TargetID     int64     `gorm:"column:target_id;not null;index:idx_audit_logs_target,sort:desc" json:"target_id"`
	Before       JSONMap   `gorm:"column:before;type:json" json:"before,omitempty"`
	After        JSONMap   `gorm:"column:after;type:json" json:"after,omitempty"`
	Status       int32     `gorm:"column:status;type:smallint;not null" json:"status"`
	ErrorMessage string    `gorm:"column:error_message;type:text" json:"error_message,omitempty"`
	RequestID    string    `gorm:"column:request_id;type:varchar(64)" json:"request_id,omitempty"`
	CreatedAt    time.Time `gorm:"column:created_at;not null;autoCreateTime;index:idx_audit_logs_created,sort:desc" json:"created_at"`
}

// JSONMap is a custom type for JSONB map fields with any values.
type JSONMap map[string]any

// Value implements the driver.Valuer interface.
func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return jsonx.Marshal(m)
}

// Scan implements the sql.Scanner interface. Handles both []byte
// (postgres/mysql) and string (sqlite/modernc) for cross-dialect portability.
func (m *JSONMap) Scan(value any) error {
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
		return fmt.Errorf("cannot scan %T into JSONMap", value)
	}
	return jsonx.Unmarshal(bytes, m)
}
