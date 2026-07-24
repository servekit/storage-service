package handler

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
)

// Upload RPCs

// GenerateUploadURL issues a one-shot upload URL (or STS-credentialled upload
// session) for a caller to push an object to object storage.
func (h *Handler) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	return h.svc.GenerateUploadURL(ctx, req)
}

// ConfirmUpload finalizes an upload session: verifies the object landed in
// storage, records the file metadata, and consumes quota.
func (h *Handler) ConfirmUpload(ctx context.Context, req *storagev1.ConfirmUploadRequest) (*storagev1.ConfirmUploadResponse, error) {
	return h.svc.ConfirmUpload(ctx, req)
}

// CancelUpload aborts a pending upload session and releases any held quota.
func (h *Handler) CancelUpload(ctx context.Context, req *storagev1.CancelUploadRequest) (*emptypb.Empty, error) {
	return h.svc.CancelUpload(ctx, req)
}

// GetSTSCredential returns a short-lived STS credential scoped to a single
// object key for direct-to-provider uploads.
func (h *Handler) GetSTSCredential(ctx context.Context, req *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	return h.svc.GetSTSCredential(ctx, req)
}

// BatchGetSTSCredential issues STS credentials for a batch of objects in one
// round-trip (multi-file upload UX).
func (h *Handler) BatchGetSTSCredential(ctx context.Context, req *storagev1.BatchGetSTSCredentialRequest) (*storagev1.BatchGetSTSCredentialResponse, error) {
	return h.svc.BatchGetSTSCredential(ctx, req)
}

// File RPCs

// GenerateDownloadURL returns a pre-signed download URL for a file the caller
// owns (or is allowed to read).
func (h *Handler) GenerateDownloadURL(ctx context.Context, req *storagev1.GenerateDownloadURLRequest) (*storagev1.GenerateDownloadURLResponse, error) {
	return h.svc.GenerateDownloadURL(ctx, req)
}

// ListMyFiles lists files owned by the caller with pagination.
func (h *Handler) ListMyFiles(ctx context.Context, req *storagev1.ListMyFilesRequest) (*storagev1.ListMyFilesResponse, error) {
	return h.svc.ListMyFiles(ctx, req)
}

// ListMyFilesPaged lists files owned by the caller with offset pagination.
func (h *Handler) ListMyFilesPaged(ctx context.Context, req *storagev1.ListMyFilesPagedRequest) (*storagev1.ListMyFilesPagedResponse, error) {
	return h.svc.ListMyFilesPaged(ctx, req)
}

// GetMyFile returns metadata for a single file owned by the caller.
func (h *Handler) GetMyFile(ctx context.Context, req *storagev1.GetMyFileRequest) (*storagev1.UserFileInfo, error) {
	return h.svc.GetMyFile(ctx, req)
}

// UpdateMyFile updates editable file metadata (description, is_public, etc.).
func (h *Handler) UpdateMyFile(ctx context.Context, req *storagev1.UpdateMyFileRequest) (*storagev1.UserFileInfo, error) {
	return h.svc.UpdateMyFile(ctx, req)
}

// DeleteMyFile soft-deletes a file owned by the caller and releases quota.
func (h *Handler) DeleteMyFile(ctx context.Context, req *storagev1.DeleteMyFileRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteMyFile(ctx, req)
}

// BatchDeleteMyFiles soft-deletes a batch of files owned by the caller.
func (h *Handler) BatchDeleteMyFiles(ctx context.Context, req *storagev1.BatchDeleteMyFilesRequest) (*storagev1.BatchDeleteMyFilesResponse, error) {
	return h.svc.BatchDeleteMyFiles(ctx, req)
}

// GenerateProcessURL returns a pre-signed URL for image/video processing
// pipelines (provider-specific transform params encoded in the URL).
func (h *Handler) GenerateProcessURL(ctx context.Context, req *storagev1.GenerateProcessURLRequest) (*storagev1.GenerateProcessURLResponse, error) {
	return h.svc.GenerateProcessURL(ctx, req)
}

// GenerateCDNURL returns a CDN-fronted signed URL (with expiry) for an
// already-uploaded file. Caller must own the file.
func (h *Handler) GenerateCDNURL(ctx context.Context, req *storagev1.GenerateCDNURLRequest) (*storagev1.GenerateCDNURLResponse, error) {
	return h.svc.GenerateCDNURL(ctx, req)
}

// Quota RPCs

// GetMyQuota returns the caller's current storage quota and usage.
func (h *Handler) GetMyQuota(ctx context.Context, req *storagev1.GetMyQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.GetMyQuota(ctx, req)
}

// SetOwnerQuota sets an owner's total storage quota (admin/business caller).
func (h *Handler) SetOwnerQuota(ctx context.Context, req *storagev1.SetOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.SetOwnerQuota(ctx, req)
}

// AddOwnerQuota atomically increments an owner's total storage quota.
func (h *Handler) AddOwnerQuota(ctx context.Context, req *storagev1.AddOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.AddOwnerQuota(ctx, req)
}

// Admin RPCs

// AdminGetQuota returns an owner's quota and usage (admin view).
func (h *Handler) AdminGetQuota(ctx context.Context, req *storagev1.AdminGetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.AdminGetQuota(ctx, req)
}

// AdminSetQuota sets an owner's total storage quota (admin override).
func (h *Handler) AdminSetQuota(ctx context.Context, req *storagev1.AdminSetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.AdminSetQuota(ctx, req)
}

// AdminSoftDeleteOwnerFiles soft-deletes every file owned by an owner.
func (h *Handler) AdminSoftDeleteOwnerFiles(ctx context.Context, req *storagev1.AdminSoftDeleteOwnerFilesRequest) (*storagev1.AdminSoftDeleteOwnerFilesResponse, error) {
	return h.svc.AdminSoftDeleteOwnerFiles(ctx, req)
}

// AdminDeleteOwner hard-deletes an owner and cascades file cleanup.
func (h *Handler) AdminDeleteOwner(ctx context.Context, req *storagev1.AdminDeleteOwnerRequest) (*storagev1.AdminDeleteOwnerResponse, error) {
	return h.svc.AdminDeleteOwner(ctx, req)
}

// AdminGetStats returns aggregate storage stats (file counts, bytes) for admins.
func (h *Handler) AdminGetStats(ctx context.Context, req *storagev1.AdminGetStatsRequest) (*storagev1.AdminGetStatsResponse, error) {
	return h.svc.AdminGetStats(ctx, req)
}

// AdminListFiles lists files across all owners (admin view, with filters).
func (h *Handler) AdminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	return h.svc.AdminListFiles(ctx, req)
}

// AdminGetFile returns metadata for a single file (admin view).
func (h *Handler) AdminGetFile(ctx context.Context, req *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	return h.svc.AdminGetFile(ctx, req)
}

// AdminDeleteFile hard-deletes a single file (admin override).
func (h *Handler) AdminDeleteFile(ctx context.Context, req *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	return h.svc.AdminDeleteFile(ctx, req)
}

// AdminListProviders lists configured storage providers (admin diagnostic).
func (h *Handler) AdminListProviders(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	return h.svc.AdminListProviders(ctx, req)
}

// AdminListBuckets lists buckets per provider (admin diagnostic).
func (h *Handler) AdminListBuckets(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	return h.svc.AdminListBuckets(ctx, req)
}

// Audit Log RPCs

// ListMyAuditLogs returns audit log entries scoped to the caller.
func (h *Handler) ListMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	return h.svc.ListMyAuditLogs(ctx, req)
}

// AdminListAuditLogs returns audit log entries across all owners (admin view).
func (h *Handler) AdminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	return h.svc.AdminListAuditLogs(ctx, req)
}
