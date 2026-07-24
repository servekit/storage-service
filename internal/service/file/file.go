// Package file implements the file-management domain: CRUD operations on
// user-owned files, plus URL generation for download and image processing.
//
// The Service depends on audit.Recorder (to log file mutations) and the quota
// Service (to release quota when files are deleted). Both are injected via Deps
// so this package never imports the parent service package.
package file

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/servekit/go-common/dbx"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/service/quota"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/internal/utils/pagination"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/thirdcall"
	"github.com/servekit/storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)

// Service holds file-domain dependencies.
type Service struct {
	db       *gorm.DB
	gid      thirdcall.GIDService
	registry *storage.Registry
	audit    audit.Recorder
	quota    *quota.Service
	cdn      config.CDNRuntimeConfig
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB       *gorm.DB
	GID      thirdcall.GIDService
	Registry *storage.Registry
	Audit    audit.Recorder
	Quota    *quota.Service
	CDN      config.CDNRuntimeConfig
}

// New constructs a file.Service.
func New(d *Deps) *Service {
	return &Service{
		db:       d.DB,
		gid:      d.GID,
		registry: d.Registry,
		audit:    d.Audit,
		quota:    d.Quota,
		cdn:      d.CDN,
	}
}

// GenerateDownloadURL returns a pre-signed download URL for a file owned by
// the caller (or otherwise accessible). URL TTL and response-content-disposition
// are taken from the request.
func (s *Service) GenerateDownloadURL(ctx context.Context, req *storagev1.GenerateDownloadURLRequest) (*storagev1.GenerateDownloadURLResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	p, err := s.registry.ProviderForBucket(obj.Bucket)
	if err != nil {
		return nil, xcodes.ErrProviderNotFound.Wrap(err)
	}

	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	// Public objects live in public-read buckets and are reachable via an
	// unsigned URL — no presign needed. The provider returns an unsigned URL
	// when WithPublic() is passed; for private objects the normal presigned
	// URL is generated. obj.IsPublic is the persisted bucket-ACL derivation
	// captured at upload time, so callers always see the correct visibility.
	var presignOpts []types.GetPresignOption
	if obj.IsPublic {
		presignOpts = append(presignOpts, storage.WithPublic())
	}
	// Caller-supplied filename overrides the stored one — supports "save as"
	// flows where the user wants the download under a different name than the
	// original upload. Falls back to uf.Filename for backwards-compat.
	filename := uf.Filename
	if fn := req.GetFilename(); fn != "" {
		filename = fn
	}
	presignOpts = append(presignOpts, storage.WithDownloadFilename(filename))

	downloadURL, err := p.PresignGetObject(ctx, obj.Bucket, obj.ObjectKey, ttl, presignOpts...)
	if err != nil {
		return nil, fmt.Errorf("presign get object: %w", err)
	}

	expiresAt := time.Now().Add(ttl)

	return &storagev1.GenerateDownloadURLResponse{
		DownloadUrl: downloadURL,
		ExpiresAt:   expiresAt.Unix(),
	}, nil
}

// ListMyFiles lists files owned by the caller with pagination and optional
// filters (filename substring, is_public flag, MIME-type prefix).
func (s *Service) ListMyFiles(ctx context.Context, req *storagev1.ListMyFilesRequest) (*storagev1.ListMyFilesResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	filter := dal.ListFilesFilter{
		PathPrefix:        req.GetPathPrefix(),
		Extension:         req.GetExtension(),
		ContentTypePrefix: req.GetContentTypePrefix(),
		OrderBy:           req.GetOrderBy(),
		Descending:        req.GetDescending(),
		Pagination: dbx.Pagination{
			PageSize: int(req.GetPageSize()),
		},
	}

	// Parse cursor: page token carries (sort_value, id) so non-id ORDER BY
	// columns page without dropping or duplicating rows. Legacy bare-numeric
	// tokens are accepted for backwards compat.
	if token := req.GetPageToken(); token != "" {
		cursor, err := pagination.DecodePageCursor(token)
		if err == nil {
			filter.AfterID = cursor.ID
			filter.AfterFilename = cursor.Filename
			filter.AfterCreatedAt = pagination.CursorToCreatedAt(cursor.CreatedAt)
		}
	}

	// Cross-table filter resolved at service layer: look up object IDs first.
	var objectIDs []int64
	if req.GetContentTypePrefix() != "" {
		ids, err := dal.FindObjectIDsByContentTypePrefix(ctx, s.db, req.GetContentTypePrefix())
		if err != nil {
			return nil, fmt.Errorf("find object ids by content type: %w", err)
		}
		if len(ids) == 0 {
			// No matching objects → no matching files; return empty response.
			return &storagev1.ListMyFilesResponse{}, nil
		}
		objectIDs = ids
	}

	files, err := dal.ListFilesByOwner(ctx, s.db, ownerID, ownerType, filter, objectIDs)
	if err != nil {
		return nil, fmt.Errorf("list user files: %w", err)
	}

	pg := filter.Normalize()

	// Batch-fetch objects instead of N+1 queries.
	fileObjectIDs := make([]int64, 0, len(files))
	for _, f := range files {
		fileObjectIDs = append(fileObjectIDs, f.ObjectID)
	}
	objectsMap, err := dal.BatchGetObjectsByIDs(ctx, s.db, fileObjectIDs)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	protoFiles := make([]*storagev1.UserFileInfo, 0, len(files))
	for i := range files {
		obj := objectsMap[files[i].ObjectID]
		if obj == nil {
			obj = &models.StorageObject{}
		}
		protoFiles = append(protoFiles, buildUserFileInfo(&files[i], obj))
	}

	// Trim to page size and detect next page.
	protoFiles, hasNext := dbx.TrimPage(protoFiles, pg.PageSize)

	var nextPageToken string
	if hasNext {
		lastFile := files[pg.PageSize-1]
		nextPageToken = pagination.EncodePageCursor(pagination.PageCursor{
			ID:        lastFile.ID,
			Filename:  lastFile.Filename,
			CreatedAt: pagination.CursorFromCreatedAt(lastFile.CreatedAt),
		})
	}

	return &storagev1.ListMyFilesResponse{
		Files:         protoFiles,
		NextPageToken: nextPageToken,
	}, nil
}

// ListMyFilesPaged lists files owned by the caller using traditional offset
// pagination. Returns total_count, page, and total_pages so UIs can render
// page navigation. For stable iteration (background scans, exports), use
// ListMyFiles instead — it's cheaper (no COUNT) and stable under writes.
func (s *Service) ListMyFilesPaged(ctx context.Context, req *storagev1.ListMyFilesPagedRequest) (*storagev1.ListMyFilesPagedResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	// Validate offset pagination bounds. dbx.ClampPageSize would silently
	// clamp, but for an explicit pagination API the caller deserves a hard
	// error so they know their input is wrong.
	if req.GetPage() < 1 {
		return nil, xcodes.ErrBadRequest.New("page must be >= 1")
	}
	if req.GetPageSize() < 1 || int(req.GetPageSize()) > dbx.MaxPageSize {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("page_size must be in [1, %d]", dbx.MaxPageSize))
	}

	// Cross-table filter resolved at service layer.
	var objectIDs []int64
	if req.GetContentTypePrefix() != "" {
		ids, err := dal.FindObjectIDsByContentTypePrefix(ctx, s.db, req.GetContentTypePrefix())
		if err != nil {
			return nil, fmt.Errorf("find object ids by content type: %w", err)
		}
		if len(ids) == 0 {
			return &storagev1.ListMyFilesPagedResponse{
				Page:       req.GetPage(),
				TotalPages: 0,
				HasMore:    false,
			}, nil
		}
		objectIDs = ids
	}

	filter := dal.ListFilesPagedFilter{
		PathPrefix: req.GetPathPrefix(),
		Extension:  req.GetExtension(),
		OrderBy:    req.GetOrderBy(),
		Descending: req.GetDescending(),
		PageParams: dbx.PageParams{
			Page:     int(req.GetPage()),
			PageSize: int(req.GetPageSize()),
			Count:    true,
		},
	}

	files, total, err := dal.ListFilesByOwnerPaged(ctx, s.db, ownerID, ownerType, filter, objectIDs)
	if err != nil {
		return nil, fmt.Errorf("list files paged: %w", err)
	}

	// Batch-fetch objects instead of N+1.
	fileObjectIDs := make([]int64, 0, len(files))
	for _, f := range files {
		fileObjectIDs = append(fileObjectIDs, f.ObjectID)
	}
	objectsMap, err := dal.BatchGetObjectsByIDs(ctx, s.db, fileObjectIDs)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	protoFiles := make([]*storagev1.UserFileInfo, 0, len(files))
	for i := range files {
		obj := objectsMap[files[i].ObjectID]
		if obj == nil {
			obj = &models.StorageObject{}
		}
		protoFiles = append(protoFiles, buildUserFileInfo(&files[i], obj))
	}

	pageSize := int(req.GetPageSize())
	totalPages := int((total + int64(pageSize) - 1) / int64(pageSize))

	return &storagev1.ListMyFilesPagedResponse{
		Files:      protoFiles,
		TotalCount: total,
		Page:       req.GetPage(),
		TotalPages: int32(totalPages),
		HasMore:    req.GetPage() < int32(totalPages),
	}, nil
}

// GetMyFile returns metadata for a single file owned by the caller. Returns
// NotFound if the file does not exist or belongs to another owner.
func (s *Service) GetMyFile(ctx context.Context, req *storagev1.GetMyFileRequest) (*storagev1.UserFileInfo, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	return buildUserFileInfo(uf, obj), nil
}

// UpdateMyFile updates editable file metadata (filename, description,
// is_public). Records an audit event capturing before/after state.
func (s *Service) UpdateMyFile(ctx context.Context, req *storagev1.UpdateMyFileRequest) (*storagev1.UserFileInfo, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	oldValues := conv.MustToMap(audit.FileSnapshot{
		Filename:    uf.Filename,
		FilePath:    uf.FilePath,
		Description: uf.Description,
		IsPublic:    uf.IsPublic,
	})

	// Update only fields that are explicitly set via optional wrappers.
	if req.Filename != nil {
		uf.Filename = req.GetFilename()
	}
	if req.FilePath != nil {
		uf.FilePath = req.GetFilePath()
	}
	if req.Description != nil {
		uf.Description = req.GetDescription()
	}
	// metadata: proto3 cannot distinguish "field absent" from "empty map"
	// (both default to an empty map), so two signals drive the update:
	//   - len(metadata) > 0: replace metadata with the supplied entries
	//   - clear_metadata == true: wipe metadata entirely
	// Anything else (empty metadata, clear_metadata unset/false) preserves
	// the existing entries — required so a caller who only wants to rename
	// the file doesn't lose their metadata.
	if len(req.GetMetadata()) > 0 {
		uf.Metadata = models.MapJSON(req.GetMetadata())
	} else if req.GetClearMetadata() {
		uf.Metadata = models.MapJSON{}
	}
	// is_public is no longer client-settable: it is derived from the bucket
	// ACL at upload time and persisted on the StorageObject. To change a
	// file's public visibility, move it to a bucket with a different ACL.

	if updateErr := dal.UpdateFile(ctx, s.db, uf); updateErr != nil {
		s.audit.RecordOutcome(ctx, audit.Event{
			Action:     storagev1.AuditAction_AUDIT_ACTION_UPDATE,
			RequestID:  req.GetRequestId(),
			OwnerType:  ownerType,
			OwnerID:    ownerID,
			TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
			TargetID:   uf.ID,
			Before:     oldValues,
		}, updateErr)
		return nil, fmt.Errorf("update user file: %w", updateErr)
	}

	newValues := conv.MustToMap(audit.FileSnapshot{
		Filename:    uf.Filename,
		FilePath:    uf.FilePath,
		Description: uf.Description,
		IsPublic:    uf.IsPublic,
	})
	s.audit.RecordOutcome(ctx, audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_UPDATE,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   uf.ID,
		Before:     oldValues,
		After:      newValues,
	}, nil)

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	return buildUserFileInfo(uf, obj), nil
}

// DeleteMyFile soft-deletes a file owned by the caller and releases the
// consumed quota. The underlying object is purged later by the cleanup cron.
func (s *Service) DeleteMyFile(ctx context.Context, req *storagev1.DeleteMyFileRequest) (*emptypb.Empty, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	deleteBefore := conv.MustToMap(audit.FileSnapshot{Filename: uf.Filename, FilePath: uf.FilePath, Size: obj.Size})
	deleteAuditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_DELETE,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   uf.ID,
		Before:     deleteBefore,
	}
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if delErr := dal.DeleteFile(ctx, tx, uf.ID); delErr != nil {
			return delErr
		}
		if refErr := dal.DecrObjectRefCount(ctx, tx, obj.ID); refErr != nil {
			return refErr
		}
		if releaseErr := s.quota.Release(ctx, tx, ownerType, ownerID, obj.Size); releaseErr != nil {
			return releaseErr
		}
		return nil
	})
	if txErr != nil {
		s.audit.RecordOutcome(ctx, deleteAuditBase, txErr)
		return nil, fmt.Errorf("delete file transaction: %w", txErr)
	}

	s.audit.RecordOutcome(ctx, deleteAuditBase, nil)
	return &emptypb.Empty{}, nil
}

// BatchDeleteMyFiles soft-deletes a batch of files owned by the caller. The
// response reports per-file outcomes (deleted vs. failed) so partial success
// is observable.
func (s *Service) BatchDeleteMyFiles(ctx context.Context, req *storagev1.BatchDeleteMyFilesRequest) (*storagev1.BatchDeleteMyFilesResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	if len(req.GetFileIds()) == 0 {
		return &storagev1.BatchDeleteMyFilesResponse{}, nil
	}
	if len(req.GetFileIds()) > dal.MaxBatchSize {
		return nil, xcodes.ErrFileBatchTooLarge.New()
	}

	var deletedCount int32
	failedIDs := make([]int64, 0, len(req.GetFileIds()))

	for _, id := range req.GetFileIds() {
		// Each file gets its own transaction so a single failure does not roll
		// back files that were already deleted by this iteration (or by a
		// concurrent caller).
		err := s.deleteOneFileInTx(ctx, ownerID, ownerType, id)
		if err != nil {
			// Already-deleted / not-found: file is gone from this owner's
			// perspective (deleted concurrently or never existed). Report it
			// as a per-file failure but keep processing the remaining IDs.
			if errors.Is(err, xcodes.ErrFileNotFound.New()) ||
				errors.Is(err, xcodes.ErrFileNotActive.New()) {
				failedIDs = append(failedIDs, id)
				continue
			}
			// Unexpected error: log and mark this file as failed, but do not
			// abort the rest of the batch.
			slog.Error("batch delete one file failed",
				"file_id", id, "owner_id", ownerID, "owner_type", ownerType, "error", err)
			failedIDs = append(failedIDs, id)
			continue
		}
		deletedCount++
	}

	// Single audit entry summarizing the whole batch. Status is SUCCESS as
	// long as the call completed; per-file failures are captured in the
	// `failed_ids` snapshot below.
	s.audit.RecordOutcome(ctx, audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_BATCH_DELETE,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   req.GetFileIds()[0],
		Before:     conv.MustToMap(audit.FileBatchDeleteResult{FileIDs: req.GetFileIds(), DeletedCount: deletedCount, FailedIDs: failedIDs}),
	}, nil)

	return &storagev1.BatchDeleteMyFilesResponse{
		DeletedCount: deletedCount,
		FailedIds:    failedIDs,
	}, nil
}

// deleteOneFileInTx wraps a single file's soft-delete, object ref-count-decrement
// and quota release in its own transaction. Returns nil on success.
func (s *Service) deleteOneFileInTx(ctx context.Context, ownerID int64, ownerType int32, fileID int64) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		uf, err := dal.GetFileByIDAndOwner(ctx, tx, fileID, ownerID, ownerType)
		if err != nil {
			return err
		}

		obj, err := dal.GetObjectByID(ctx, tx, uf.ObjectID)
		if err != nil {
			return err
		}

		if delErr := dal.DeleteFile(ctx, tx, uf.ID); delErr != nil {
			return delErr
		}
		if refErr := dal.DecrObjectRefCount(ctx, tx, obj.ID); refErr != nil {
			return refErr
		}
		if releaseErr := s.quota.Release(ctx, tx, ownerType, ownerID, obj.Size); releaseErr != nil {
			return releaseErr
		}
		return nil
	})
}

// GenerateProcessURL returns a pre-signed URL for image/video processing
// pipelines. Provider-specific transform parameters are encoded in the URL
// query string; the storage provider executes the transform on demand.
func (s *Service) GenerateProcessURL(ctx context.Context, req *storagev1.GenerateProcessURLRequest) (*storagev1.GenerateProcessURLResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	p, err := s.registry.ProviderForBucket(obj.Bucket)
	if err != nil {
		return nil, xcodes.ErrProviderNotFound.Wrap(err)
	}

	ops := make([]types.Op, 0, len(req.GetOps()))
	for _, op := range req.GetOps() {
		ops = append(ops, conv.ProtoToImageOp(op))
	}

	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	processURL, err := p.PresignGetObject(ctx, obj.Bucket, obj.ObjectKey, ttl,
		storage.WithImageOps(ops),
		storage.WithDownloadFilename(uf.Filename),
	)
	if err != nil {
		return nil, fmt.Errorf("generate process URL: %w", err)
	}

	return &storagev1.GenerateProcessURLResponse{
		Url:       processURL,
		ExpiresAt: time.Now().Add(ttl).Unix(),
	}, nil
}

// GetCDNURL returns a CDN-fronted URL for an already-uploaded file.
//
// When request.public=false (default): the URL is signed and expires at
// (now + ttl). Image processing ops are only honored by providers whose
// CDN+origin can process images (currently Aliyun OSS+CDN).
//
// When request.public=true: the URL is unsigned (no auth_key/Signature)
// and permanent. Requires file.is_public=true; otherwise BAD_REQUEST
// (prevents accidentally exposing a private file via a public URL). TTL
// is ignored.
func (s *Service) GetCDNURL(ctx context.Context, req *storagev1.GenerateCDNURLRequest) (*storagev1.GenerateCDNURLResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	// Ownership is enforced inside GetFileByIDAndOwner: a row owned by a
	// different caller looks identical to "not found", so we don't leak
	// existence on mismatch.
	uf, err := dal.GetFileByIDAndOwner(ctx, s.db, req.GetFileId(), ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, uf.ObjectID)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	gen, err := s.registry.CDNURLGeneratorForBucket(obj.Bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	if gen == nil {
		return nil, xcodes.ErrCDNNotConfigured.New("provider for bucket %q has no CDN configured", obj.Bucket)
	}

	public := req.GetPublic()
	if public && !uf.IsPublic {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("file %d is not public; cannot generate public CDN URL", req.GetFileId()))
	}

	var ttl time.Duration
	if !public {
		ttl = s.resolveCDNTTL(req.GetTtl().AsDuration())
	}

	ops := make([]types.Op, 0, len(req.GetOps()))
	for _, p := range req.GetOps() {
		ops = append(ops, conv.ProtoToImageOp(p))
	}

	url, expiresAt, err := gen.CDNURL(ctx, obj.ObjectKey, types.CDNURLOptions{
		Ops:      ops,
		TTL:      ttl,
		Public:   public,
		Filename: req.GetFilename(),
	})
	if err != nil {
		if errors.Is(err, types.ErrCDNImageProcessingUnsupported) {
			return nil, xcodes.ErrCDNImageProcessingUnsupported.Wrap(err)
		}
		if errors.Is(err, types.ErrCDNNotConfigured) {
			return nil, xcodes.ErrCDNNotConfigured.Wrap(err)
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	// Public URLs have no expiry (provider returns the zero time.Time).
	// Surface 0 in the proto response (the "permanent" convention) instead
	// of the negative Unix timestamp of the zero time.
	var expiresAtUnix int64
	if !public {
		expiresAtUnix = expiresAt.Unix()
	}

	return &storagev1.GenerateCDNURLResponse{Url: url, ExpiresAt: expiresAtUnix}, nil
}

// resolveCDNTTL clamps ttl to [cdn.min_ttl, cdn.max_ttl]. A zero value falls
// back to cdn.default_ttl. When the CDN runtime config itself is zero-valued
// (e.g. an in-process caller that didn't wire it up), hardcoded safe defaults
// are used so the method never returns a zero-TTL URL.
func (s *Service) resolveCDNTTL(ttl time.Duration) time.Duration {
	const (
		defaultTTL = time.Hour
		minTTL     = 5 * time.Minute
		maxTTL     = 24 * time.Hour
	)

	dflt := s.cdn.DefaultTTL
	if dflt <= 0 {
		dflt = defaultTTL
	}
	mn := s.cdn.MinTTL
	if mn <= 0 {
		mn = minTTL
	}
	mx := s.cdn.MaxTTL
	if mx <= 0 {
		mx = maxTTL
	}

	if ttl <= 0 {
		return dflt
	}
	if ttl < mn {
		return mn
	}
	if ttl > mx {
		return mx
	}
	return ttl
}

// --- internal helpers ---

// buildUserFileInfo converts a File model and its StorageObject into a
// UserFileInfo proto message.
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
		OwnerType:   conv.OwnerTypeToProto(file.OwnerType),
		Size:        obj.Size,
		ContentType: obj.ContentType,
		Extension:   obj.Extension,
		Md5:         obj.MD5,
		CreatedAt:   file.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   file.UpdatedAt.Format(time.RFC3339),
	}
}
