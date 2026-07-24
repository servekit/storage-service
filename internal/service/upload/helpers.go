package upload

import (
	"maps"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/store/models"
)

// Snapshot map conversion is in internal/service/conv — see conv.MustToMap.
// FileSnapshot and SessionSnapshot types remain here for backward
// compatibility with the audit.Event shape produced before Phase 4.

// FileSnapshot captures file state for an upload audit event. Mirrors the
// service.FileSnapshot shape so the parent can re-map it onto its audit.Event
// unchanged.
type FileSnapshot struct {
	Filename    string `json:"filename"`
	FilePath    string `json:"file_path,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	MD5         string `json:"md5,omitempty"`
	IsPublic    bool   `json:"is_public,omitempty"`
}

// SessionSnapshot captures session state for an upload audit event.
// Mirrors the service.SessionSnapshot shape.
type SessionSnapshot struct {
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

// buildUserFileInfo converts a File model and its StorageObject into a
// UserFileInfo proto message. Duplicated from the parent service package.
func buildUserFileInfo(file *models.StorageFile, obj *models.StorageObject) *storagev1.UserFileInfo {
	if obj == nil {
		obj = &models.StorageObject{}
	}

	metadata := make(map[string]string, len(file.Metadata))
	maps.Copy(metadata, file.Metadata)

	return &storagev1.UserFileInfo{
		Id:          file.ID,
		Filename:    file.Filename,
		FilePath:    file.FilePath,
		Description: file.Description,
		Metadata:    metadata,
		IsPublic:    file.IsPublic,
		OwnerType:   storagev1.OwnerType(file.OwnerType),
		Size:        obj.Size,
		ContentType: obj.ContentType,
		Extension:   obj.Extension,
		Md5:         obj.MD5,
		CreatedAt:   file.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   file.UpdatedAt.Format(time.RFC3339),
	}
}
