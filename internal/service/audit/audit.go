// Package audit implements the audit-logging domain for the storage service.
// It owns the Recorder (used by every other domain to record audit events),
// the snapshot types that capture before/after state, and the two RPCs that
// read audit logs.
package audit

import (
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/thirdcall"

	"google.golang.org/protobuf/types/known/structpb"
	"gorm.io/gorm"
)

// Service holds audit-domain dependencies.
type Service struct {
	db  *gorm.DB
	gid thirdcall.GIDService
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB  *gorm.DB
	GID thirdcall.GIDService
}

// Event represents an auditable operation. Fields use proto enum types so the
// audit pipeline stays type-safe end-to-end: callers pass proto enums, the
// recorder persists the underlying int32, and the read path converts back to
// proto enums via Go type conversion (no switch needed).
type Event struct {
	Action     storagev1.AuditAction        // Operation type
	OwnerType  int32                        // Who performed the operation (proto OwnerType int32 value)
	OwnerID    int64                        // Who performed the operation
	TargetType storagev1.AuditLogTargetType // What was operated on
	TargetID   int64                        // ID of the target
	Before     map[string]any               // State before the operation (nil for creates)
	After      map[string]any               // State after the operation (nil for deletes)
	Status     storagev1.AuditLogStatus     // success or failed
	Error      error                        // Failure reason (nil for success)
	RequestID  string                       // Caller-provided request ID for traceability (empty if caller didn't set one)
}

// FileSnapshot captures file state for audit before/after. Fields use omitempty
// so different audit points can fill only the fields relevant to that operation
// (upload records size+content_type, update records is_public, etc.).
type FileSnapshot struct {
	Filename    string `json:"filename"`
	FilePath    string `json:"file_path,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	MD5         string `json:"md5,omitempty"`
	IsPublic    bool   `json:"is_public,omitempty"`
}

// QuotaSnapshot captures quota state for audit before/after.
type QuotaSnapshot struct {
	TotalBytes int64 `json:"total_bytes"`
}

// OwnerDeletionResult captures the outcome of deleting an owner's files.
type OwnerDeletionResult struct {
	FilesDeleted  int64 `json:"files_deleted"`
	BytesReleased int64 `json:"bytes_released"`
}

// FileBatchDeleteResult captures the outcome of a batch file deletion.
type FileBatchDeleteResult struct {
	FileIDs      []int64 `json:"file_ids"`
	DeletedCount int32   `json:"deleted_count"`
	FailedIDs    []int64 `json:"failed_ids,omitempty"`
}

// UploadSessionSnapshot captures session state for audit before/after. Fields
// use omitempty so different audit points can fill only the fields relevant to
// that operation (token issued vs. confirmed vs. cancelled). Vendor is always
// populated for multi-vendor debugging.
type UploadSessionSnapshot struct {
	ID        int64  `json:"id"`
	OwnerType int32  `json:"owner_type"`
	OwnerID   int64  `json:"owner_id"`
	Vendor    int32  `json:"vendor"`
	Bucket    string `json:"bucket"`
	ObjectKey string `json:"object_key"`
	MD5       string `json:"md5"`
	Size      int64  `json:"size"`
	Status    int32  `json:"status"`
	FileID    *int64 `json:"file_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// New constructs an audit.Service.
func New(d *Deps) *Service {
	return &Service{db: d.DB, gid: d.GID}
}

// Recorder returns the audit recorder. Other subpackages accept this as a
// Deps field so they can record audit events without importing this package's
// internals.
func (s *Service) Recorder() Recorder {
	return NewDBRecorder(s.db, s.gid)
}

// --- internal helpers ---

// buildAuditLogEntry converts a stored AuditLog row to its proto representation.
// Proto enum types are int32-backed, so conversion is a Go type cast — no
// switch needed.
func buildAuditLogEntry(log *models.StorageAuditLog) *storagev1.AuditLogEntry {
	entry := &storagev1.AuditLogEntry{
		Id:           log.ID,
		Action:       storagev1.AuditAction(log.Action),
		OwnerType:    conv.OwnerTypeToProto(log.OwnerType),
		OwnerId:      log.OwnerID,
		TargetType:   storagev1.AuditLogTargetType(log.TargetType),
		TargetId:     log.TargetID,
		Status:       storagev1.AuditLogStatus(log.Status),
		ErrorMessage: log.ErrorMessage,
		RequestId:    log.RequestID,
		CreatedAt:    log.CreatedAt.Format(time.RFC3339),
	}

	if log.Before != nil {
		entry.Before, _ = structpb.NewStruct(log.Before)
	}
	if log.After != nil {
		entry.After, _ = structpb.NewStruct(log.After)
	}

	return entry
}

// buildAuditLog assembles an AuditLog row from a snowflake ID and an Event.
// Shared by Record (own handle) and RecordInTx (caller's tx).
func buildAuditLog(id int64, event Event) *models.StorageAuditLog {
	var errMsg string
	if event.Error != nil {
		errMsg = event.Error.Error()
	}
	return &models.StorageAuditLog{
		ID:           id,
		Action:       int32(event.Action),
		OwnerType:    event.OwnerType,
		OwnerID:      event.OwnerID,
		TargetType:   int32(event.TargetType),
		TargetID:     event.TargetID,
		Before:       models.JSONMap(event.Before),
		After:        models.JSONMap(event.After),
		Status:       int32(event.Status),
		ErrorMessage: errMsg,
		RequestID:    event.RequestID,
	}
}
