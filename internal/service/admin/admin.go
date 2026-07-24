// Package admin implements the admin domain: cross-owner management RPCs
// (list/get/delete files, get/set quota, aggregate stats, list providers/buckets,
// soft-delete / hard-delete owners), plus the cleanup helpers that purge
// expired deleted objects and owners.
//
// The Service depends on audit.Recorder (to log admin mutations) and the quota
// Service (to read/release quota). Both are injected via Deps so this package
// never imports the parent service package.
package admin

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/servekit/go-common/dbx"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/service/quota"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/thirdcall"
	"github.com/servekit/storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)

// Service holds admin-domain dependencies.
type Service struct {
	db       *gorm.DB
	gid      thirdcall.GIDService
	registry *storage.Registry
	audit    audit.Recorder
	quota    *quota.Service
	cfg      *config.Config
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB       *gorm.DB
	GID      thirdcall.GIDService
	Registry *storage.Registry
	Audit    audit.Recorder
	Quota    *quota.Service
	Cfg      *config.Config
}

// New constructs an admin.Service.
func New(d *Deps) *Service {
	return &Service{
		db:       d.DB,
		gid:      d.GID,
		registry: d.Registry,
		audit:    d.Audit,
		quota:    d.Quota,
		cfg:      d.Cfg,
	}
}

// AdminGetQuota returns an owner's storage quota and usage (admin view). The
// quota row is created on first reference if it does not yet exist.
func (s *Service) AdminGetQuota(ctx context.Context, req *storagev1.AdminGetQuotaRequest) (*storagev1.QuotaInfo, error) {
	ownerType := int32(req.GetOwnerType())
	ownerID := req.GetOwnerId()

	q, err := s.quota.GetQuota(ctx, s.db, ownerType, ownerID)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	fileCount, err := dal.CountFilesByOwner(ctx, s.db, ownerID, ownerType)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &storagev1.QuotaInfo{
		TotalBytes:     q.TotalBytes,
		UsedBytes:      q.UsedBytes,
		AvailableBytes: q.TotalBytes - q.UsedBytes,
		FileCount:      int32(fileCount),
	}, nil
}

// AdminSetQuota sets an owner's total storage quota (admin override) and
// returns the resulting quota row. Records an audit event capturing
// before/after state inside the same transaction.
func (s *Service) AdminSetQuota(ctx context.Context, req *storagev1.AdminSetQuotaRequest) (*storagev1.QuotaInfo, error) {
	ownerType := int32(req.GetOwnerType())
	ownerID := req.GetOwnerId()

	oldQuota, oldQuotaErr := s.quota.GetQuota(ctx, s.db, ownerType, ownerID)
	var oldTotalBytes int64
	if oldQuotaErr == nil {
		oldTotalBytes = oldQuota.TotalBytes
	}

	auditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_ADMIN_SET_QUOTA,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_QUOTA,
		TargetID:   ownerID,
		Before:     conv.MustToMap(audit.QuotaSnapshot{TotalBytes: oldTotalBytes}),
		After:      conv.MustToMap(audit.QuotaSnapshot{TotalBytes: req.GetTotalBytes()}),
	}

	var result *storagev1.QuotaInfo
	// Audit is written inside the tx: an admin quota change is a compliance-
	// sensitive operation, and the audit row must not survive a rolled-back
	// write (and a rolled-back audit must not survive a committed write).
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.quota.SetQuota(ctx, tx, ownerType, ownerID, req.GetTotalBytes()); err != nil {
			s.audit.RecordOutcomeInTx(ctx, tx, auditBase, err)
			return err
		}

		q, err := s.quota.GetQuota(ctx, tx, ownerType, ownerID)
		if err != nil {
			s.audit.RecordOutcomeInTx(ctx, tx, auditBase, err)
			return err
		}
		fileCount, err := dal.CountFilesByOwner(ctx, tx, ownerID, ownerType)
		if err != nil {
			s.audit.RecordOutcomeInTx(ctx, tx, auditBase, err)
			return err
		}
		result = &storagev1.QuotaInfo{
			TotalBytes:     q.TotalBytes,
			UsedBytes:      q.UsedBytes,
			AvailableBytes: q.TotalBytes - q.UsedBytes,
			FileCount:      int32(fileCount),
		}
		s.audit.RecordOutcomeInTx(ctx, tx, auditBase, nil)
		return nil
	})
	if txErr != nil {
		return nil, xcodes.ErrInternal.Wrap(txErr)
	}
	return result, nil
}

// AdminSoftDeleteOwnerFiles soft-deletes every active file owned by an owner
// and releases the consumed quota. Returns counts; underlying objects are
// purged later by the cleanup cron. Records an audit event.
func (s *Service) AdminSoftDeleteOwnerFiles(ctx context.Context, req *storagev1.AdminSoftDeleteOwnerFilesRequest) (*storagev1.AdminSoftDeleteOwnerFilesResponse, error) {
	ownerType := int32(req.GetOwnerType())
	ownerID := req.GetOwnerId()

	auditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_ADMIN_SOFT_DELETE_OWNER,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_OWNER,
		TargetID:   ownerID,
	}

	filesDeleted, bytesReleased, err := s.softDeleteOwnerFilesInTx(ctx, ownerType, ownerID)
	if err != nil {
		s.audit.RecordOutcome(ctx, auditBase, err)
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	auditBase.Before = conv.MustToMap(audit.OwnerDeletionResult{FilesDeleted: filesDeleted, BytesReleased: bytesReleased})
	s.audit.RecordOutcome(ctx, auditBase, nil)

	return &storagev1.AdminSoftDeleteOwnerFilesResponse{
		FilesDeleted:  filesDeleted,
		BytesReleased: bytesReleased,
	}, nil
}

// AdminDeleteOwner hard-deletes an owner's quota row and soft-deletes all
// owned files in one transaction. Records an audit event.
func (s *Service) AdminDeleteOwner(ctx context.Context, req *storagev1.AdminDeleteOwnerRequest) (*storagev1.AdminDeleteOwnerResponse, error) {
	ownerType := int32(req.GetOwnerType())
	ownerID := req.GetOwnerId()

	auditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_OWNER,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_OWNER,
		TargetID:   ownerID,
	}

	var filesDeleted int64
	var bytesReleased int64

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		fd, br, err := s.softDeleteOwnerFiles(ctx, tx, ownerType, ownerID)
		if err != nil {
			return err
		}
		filesDeleted = fd
		bytesReleased = br
		return dal.DeleteQuotaByOwner(ctx, tx, ownerType, ownerID)
	})
	if txErr != nil {
		s.audit.RecordOutcome(ctx, auditBase, txErr)
		return nil, xcodes.ErrInternal.Wrap(txErr)
	}

	auditBase.Before = conv.MustToMap(audit.OwnerDeletionResult{FilesDeleted: filesDeleted, BytesReleased: bytesReleased})
	s.audit.RecordOutcome(ctx, auditBase, nil)

	return &storagev1.AdminDeleteOwnerResponse{
		FilesDeleted:  filesDeleted,
		BytesReleased: bytesReleased,
	}, nil
}

// AdminGetStats returns aggregate storage statistics (total objects/files,
// physical/logical bytes, per-owner / per-provider / per-bucket breakdowns)
// for admin dashboards.
func (s *Service) AdminGetStats(ctx context.Context, req *storagev1.AdminGetStatsRequest) (*storagev1.AdminGetStatsResponse, error) {
	stats, err := s.getStorageStats(ctx, int32(req.GetOwnerType()), req.GetOwnerId())
	if err != nil {
		return nil, fmt.Errorf("get stats: %w", err)
	}

	resp := &storagev1.AdminGetStatsResponse{
		TotalObjects:  stats.TotalObjects,
		TotalFiles:    stats.TotalFiles,
		PhysicalBytes: stats.PhysicalBytes,
		LogicalBytes:  stats.LogicalBytes,
	}

	for _, os := range stats.OwnerStats {
		resp.OwnerStats = append(resp.OwnerStats, &storagev1.OwnerStats{
			OwnerType:  storagev1.OwnerType(os.OwnerType),
			FileCount:  os.FileCount,
			TotalBytes: os.TotalBytes,
		})
	}

	for _, ps := range stats.ProviderStats {
		resp.ProviderStats = append(resp.ProviderStats, &storagev1.ProviderStats{
			Provider:    conv.VendorToName(ps.Vendor),
			ObjectCount: ps.ObjectCount,
			TotalBytes:  ps.TotalBytes,
		})
	}

	for _, bs := range stats.BucketStats {
		resp.BucketStats = append(resp.BucketStats, &storagev1.BucketStats{
			Bucket:      bs.Bucket,
			ObjectCount: bs.ObjectCount,
			TotalBytes:  bs.TotalBytes,
			FileCount:   bs.FileCount,
		})
	}

	return resp, nil
}

// AdminListFiles lists files across all owners (admin view) with filters:
// owner, path prefix, extension, content-type prefix, vendor/bucket. Cursor
// pagination via opaque page tokens.
func (s *Service) AdminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	// The StorageObject no longer has a `provider` string column; it stores a
	// `vendor` int32 enum. Resolve the legacy provider filter string to a
	// Vendor enum value via the proto name map (e.g. "VENDOR_AWS_S3" -> 2).
	// Unknown / empty values mean "no filter".
	var vendor int32
	if name := req.GetProvider(); name != "" {
		if v, ok := storagev1.Vendor_value[name]; ok {
			vendor = v
		}
	}

	filter := dal.AdminListFilesFilter{
		OwnerType:         int32(req.GetOwnerType()),
		OwnerID:           req.GetOwnerId(),
		PathPrefix:        req.GetPathPrefix(),
		Extension:         req.GetExtension(),
		ContentTypePrefix: req.GetContentTypePrefix(),
		Vendor:            vendor,
		Bucket:            req.GetBucket(),
		OrderBy:           req.GetOrderBy(),
		Descending:        req.GetDescending(),
		Pagination: dbx.Pagination{
			PageSize: int(req.GetPageSize()),
		},
	}

	if token := req.GetPageToken(); token != "" {
		if id, err := strconv.ParseInt(token, 10, 64); err == nil {
			filter.AfterID = id
		}
	}

	// Cross-table filters (ContentTypePrefix / Vendor / Bucket) live on
	// storage_objects, not files. Resolve them to a set of object IDs at the
	// service layer so FileRepo.ListAll can join via a single int64 slice.
	var prefilterObjectIDs []int64
	needObjectJoin := req.GetContentTypePrefix() != "" || vendor != 0 || req.GetBucket() != ""
	if needObjectJoin {
		ids, err := dal.FindObjectIDsByFilter(ctx, s.db, req.GetContentTypePrefix(), vendor, req.GetBucket())
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if len(ids) == 0 {
			return &storagev1.AdminListFilesResponse{}, nil
		}
		prefilterObjectIDs = ids
	}

	files, total, err := dal.ListAllFiles(ctx, s.db, filter, prefilterObjectIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	pg := filter.Normalize()

	objectIDs := make([]int64, 0, len(files))
	for _, f := range files {
		objectIDs = append(objectIDs, f.ObjectID)
	}
	objectsMap, err := dal.BatchGetObjectsByIDs(ctx, s.db, objectIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	protoFiles := make([]*storagev1.AdminFileInfo, 0, len(files))
	for i := range files {
		obj := objectsMap[files[i].ObjectID]
		protoFiles = append(protoFiles, buildAdminFileInfo(&files[i], obj))
	}

	protoFiles, hasNext := dbx.TrimPage(protoFiles, pg.PageSize)

	var nextPageToken string
	if hasNext {
		lastFile := files[pg.PageSize-1]
		nextPageToken = fmt.Sprintf("%d", lastFile.ID)
	}

	return &storagev1.AdminListFilesResponse{
		Files:         protoFiles,
		TotalCount:    int32(total),
		NextPageToken: nextPageToken,
	}, nil
}

// AdminGetFile returns full metadata for a single file (admin view, includes
// provider/bucket internals from the underlying storage object).
func (s *Service) AdminGetFile(ctx context.Context, req *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	f, err := dal.GetFileByID(ctx, s.db, req.GetFileId())
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, f.ObjectID)
	if err != nil {
		return nil, xcodes.ErrObjectNotFound.Wrap(err)
	}

	return buildAdminFileInfo(f, obj), nil
}

// AdminDeleteFile hard-deletes a single file row, decrements the object's
// refcount, and releases the consumed quota — all in one transaction. Admin
// override that bypasses soft-delete. Records an audit event.
func (s *Service) AdminDeleteFile(ctx context.Context, req *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	f, err := dal.GetFileByID(ctx, s.db, req.GetFileId())
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := dal.GetObjectByID(ctx, s.db, f.ObjectID)
	if err != nil {
		return nil, xcodes.ErrObjectNotFound.Wrap(err)
	}

	auditBase := audit.Event{
		Action:     storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE,
		RequestID:  req.GetRequestId(),
		OwnerType:  f.OwnerType,
		OwnerID:    f.OwnerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   f.ID,
		Before:     conv.MustToMap(audit.FileSnapshot{Filename: f.Filename, FilePath: f.FilePath, Size: obj.Size}),
	}
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if delErr := dal.DeleteFile(ctx, tx, f.ID); delErr != nil {
			return delErr
		}
		if refErr := dal.DecrObjectRefCount(ctx, tx, obj.ID); refErr != nil {
			return refErr
		}
		if releaseErr := s.quota.Release(ctx, tx, f.OwnerType, f.OwnerID, obj.Size); releaseErr != nil {
			return releaseErr
		}
		return nil
	})
	if txErr != nil {
		s.audit.RecordOutcome(ctx, auditBase, txErr)
		return nil, xcodes.ErrInternal.Wrap(txErr)
	}

	s.audit.RecordOutcome(ctx, auditBase, nil)
	return &emptypb.Empty{}, nil
}

// AdminListProviders lists configured storage providers (name, vendor,
// endpoint, region) for admin diagnostics.
func (s *Service) AdminListProviders(_ context.Context, _ *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	entries := s.registry.AllProviders()

	providers := make([]*storagev1.ProviderInfo, 0, len(entries))
	for _, e := range entries {
		v, ok := storagev1.Vendor_value[e.Vendor]
		if !ok {
			v = int32(storagev1.Vendor_VENDOR_UNSPECIFIED)
		}
		providers = append(providers, &storagev1.ProviderInfo{
			Name:     e.Name,
			Vendor:   storagev1.Vendor(v),
			Endpoint: e.Endpoint,
			Region:   e.Region,
		})
	}

	return &storagev1.AdminListProvidersResponse{Providers: providers}, nil
}

// AdminListBuckets lists configured buckets per provider (name, key prefix,
// ACL, vendor) for admin diagnostics.
func (s *Service) AdminListBuckets(_ context.Context, _ *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	entries := s.registry.AllBuckets()

	buckets := make([]*storagev1.BucketInfo, 0, len(entries))
	for _, e := range entries {
		buckets = append(buckets, &storagev1.BucketInfo{
			Name:      e.Name,
			Provider:  e.Provider,
			KeyPrefix: e.KeyPrefix,
			Acl:       conv.ACLToProto(e.ACL),
			Vendor:    s.registry.VendorForBucket(e.Name),
		})
	}

	return &storagev1.AdminListBucketsResponse{Buckets: buckets}, nil
}

// --- internal helpers ---

// buildAdminFileInfo converts a File model and its StorageObject into an
// AdminFileInfo proto message.
func buildAdminFileInfo(file *models.StorageFile, obj *models.StorageObject) *storagev1.AdminFileInfo {
	if obj == nil {
		obj = &models.StorageObject{}
	}

	metadata := make(map[string]string, len(file.Metadata))
	maps.Copy(metadata, file.Metadata)

	return &storagev1.AdminFileInfo{
		Id:          file.ID,
		OwnerType:   conv.OwnerTypeToProto(file.OwnerType),
		OwnerId:     file.OwnerID,
		Filename:    file.Filename,
		FilePath:    file.FilePath,
		Description: file.Description,
		Metadata:    metadata,
		IsPublic:    file.IsPublic,
		ObjectId:    file.ObjectID,
		Size:        obj.Size,
		ContentType: obj.ContentType,
		Extension:   obj.Extension,
		Md5:         obj.MD5,
		Provider:    conv.VendorToName(obj.Vendor),
		Bucket:      obj.Bucket,
		ObjectKey:   obj.ObjectKey,
		CreatedAt:   file.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   file.UpdatedAt.Format(time.RFC3339),
	}
}

// softDeleteOwnerFilesInTx soft-deletes all files for an owner within a transaction.
// Returns (filesDeleted, bytesReleased, error).
func (s *Service) softDeleteOwnerFilesInTx(ctx context.Context, ownerType int32, ownerID int64) (int64, int64, error) {
	var filesDeleted int64
	var bytesReleased int64

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		fd, br, err := s.softDeleteOwnerFiles(ctx, tx, ownerType, ownerID)
		if err != nil {
			return err
		}
		filesDeleted = fd
		bytesReleased = br
		return nil
	})

	return filesDeleted, bytesReleased, txErr
}

// softDeleteOwnerFiles soft-deletes all files for an owner using the given db (may be a tx).
// Returns (filesDeleted, bytesReleased, error).
func (s *Service) softDeleteOwnerFiles(ctx context.Context, db *gorm.DB, ownerType int32, ownerID int64) (int64, int64, error) {
	// Get objectID -> refCount mapping before deleting.
	refCounts, err := dal.GetFileObjectRefCountsByOwner(ctx, db, ownerType, ownerID)
	if err != nil {
		return 0, 0, err
	}
	if len(refCounts) == 0 {
		return 0, 0, nil
	}

	// Get object sizes.
	objectIDs := make([]int64, 0, len(refCounts))
	for id := range refCounts {
		objectIDs = append(objectIDs, id)
	}
	objects, err := dal.BatchGetObjectsByIDs(ctx, db, objectIDs)
	if err != nil {
		return 0, 0, err
	}

	// Calculate total bytes to release.
	var totalBytes int64
	for objID, count := range refCounts {
		if obj, ok := objects[objID]; ok {
			totalBytes += obj.Size * count
		}
	}

	// Soft-delete all files.
	deleted, err := dal.DeleteFilesByOwner(ctx, db, ownerType, ownerID)
	if err != nil {
		return 0, 0, err
	}

	// Decrement ref counts.
	for objID, count := range refCounts {
		if err := dal.DecrObjectRefCountBy(ctx, db, objID, count); err != nil {
			return 0, 0, err
		}
	}

	// Release quota.
	if totalBytes > 0 {
		if err := s.quota.Release(ctx, db, ownerType, ownerID, totalBytes); err != nil {
			return 0, 0, fmt.Errorf("release quota: %w", err)
		}
	}

	return deleted, totalBytes, nil
}
