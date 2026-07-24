// Package upload implements the upload-session domain for the storage service:
// STS-credential issuance, upload-token signing/verification, upload
// confirmation, batch issuance, cancellation, and the periodic upload-session
// GC. Extracted from the parent service package per golang-service-development
// skill §2 (single domain = single subpackage when the domain outgrows one file).
package upload

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/service/sts"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/thirdcall"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/servekit/go-common/ratelimit"
	"github.com/servekit/go-common/redisx"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Service holds upload-domain dependencies. Constructed by the parent
// internal/service package via New and embedded as a field on StorageService.
// Resource handles (db, redis, registry) are owned by the parent — Service
// holds pointers but owns no lifecycle.
type Service struct {
	db       *gorm.DB
	registry *storage.Registry
	gid      thirdcall.GIDService
	cfg      *config.Config
	limiter  ratelimit.Limiter

	sts       *sts.Service
	dedupLock DedupLocker
	host      Host
}

// Host is the parent-service bridge: upload needs a handful of cross-domain
// operations (quota check/reserve, audit recording) that live on the parent
// StorageService and touch its full dependency set (gid, audit pipeline). Rather
// than re-implement them here and risk drift, we call back through this
// interface. The parent *service.StorageService satisfies it; the upload package
// never imports internal/service, so there is no import cycle.
type Host interface {
	// CheckQuota verifies the owner has enough remaining quota for the given
	// bytes (read-only; no DB write). db is passed by the caller so checks can
	// run against the same connection/tx the caller holds.
	CheckQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, requiredBytes int64) error
	// Reserve increases the owner's used quota by the given bytes. Caller wraps
	// in the same transaction as the business write.
	Reserve(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, bytes int64) error
	// RecordOutcome records an audit event derived from err (success/failed).
	RecordOutcome(ctx context.Context, event AuditEvent, err error)
}

// AuditEvent mirrors the subset of service.Event the upload domain records.
// Field set is intentionally identical so the parent can re-map it onto its own
// audit.Event without loss.
type AuditEvent struct {
	Action     storagev1.AuditAction
	OwnerType  int32
	OwnerID    int64
	TargetType storagev1.AuditLogTargetType
	TargetID   int64
	RequestID  string
	Before     map[string]any
	After      map[string]any
}

// DedupLock prevents thundering-herd concurrent session creation for the same
// (owner, md5, size). The concrete implementation DedupLock struct (defined
// below) adapts a redisx.Lock; acquire returns errDedupLockDisabled (or
// redisx.ErrLockFailed) to signal fall-through, both handled by
// findOrCreateSession. Defined as an interface so tests can inject a fake.
type DedupLocker interface {
	acquire(ctx context.Context, ownerType int32, ownerID int64, md5 string, size int64) (string, error)
	release(ctx context.Context, ownerType int32, ownerID int64, md5 string, size int64, id string)
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB        *gorm.DB
	Registry  *storage.Registry
	GID       thirdcall.GIDService
	Cfg       *config.Config
	Limiter   ratelimit.Limiter
	Redis     *redis.Client
	STS       *config.STSConfig
	DedupLock DedupLocker
	Host      Host
}

// fileMeta bundles per-file input for issueUploadCredential. Used by single and batch paths.
// isPublic is intentionally NOT a field — it is derived from the bucket ACL
// at session/object creation time, not supplied by the client.
type fileMeta struct {
	md5, filename, contentType, filePath, description string
	metadata                                          map[string]string
	size                                              int64
	requestID                                         string
}

// issueResult holds either an instant File (MD5 dedup hit) or full upload credentials.
type issueResult struct {
	instant  bool
	fileID   int64
	fileInfo *storagev1.UserFileInfo

	uploadToken, accessKey, secretKey, securityToken, endpoint, bucket, objectKey string
	expiresAt                                                                     int64
}

// errDedupLockDisabled — an intentional deployment mode where callers
// fall through to FindPendingDedup and rely on the DB-level partial unique index.
type DedupLock struct {
	lock *redisx.Lock
}

// errDedupLockDisabled is returned by DedupLock.acquire when Redis
// is not configured (lock == nil). It is distinct from a Redis outage: "no
// Redis configured" is an intentional deployment mode where callers fall
// through to FindPendingDedup and rely on the DB-level partial unique index,
// whereas a Redis outage (any other non-ErrLockFailed error) fails closed.
var errDedupLockDisabled = errors.New("upload: dedup lock disabled (no redis)")

// New constructs an upload.Service from injected deps. The STS cache is built
// internally (it adapts the registry, which is upload-domain state) and the
// dedup lock is injected so the parent can share its redisx lock config.
func New(d *Deps) *Service {
	issuer := sts.FuncIssuer(func(ctx context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
		p, err := d.Registry.ProviderForBucket(policy.Bucket)
		if err != nil {
			return nil, err
		}
		return p.GetSTSToken(ctx, policy)
	})
	return &Service{
		db:        d.DB,
		registry:  d.Registry,
		gid:       d.GID,
		cfg:       d.Cfg,
		limiter:   d.Limiter,
		sts:       sts.New(d.Redis, issuer, d.STS),
		dedupLock: d.DedupLock,
		host:      d.Host,
	}
}

// GenerateUploadURL reserves quota, creates an upload session row, and returns
// either a direct upload URL or STS credentials (depending on provider) so the
// client can push the object to object storage. Enforces per-owner upload rate
// limits.
func (s *Service) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	if err := s.checkUploadRateLimit(ctx, ownerType, ownerID); err != nil {
		return nil, err
	}

	if checkErr := s.host.CheckQuota(ctx, s.db, ownerType, ownerID, req.GetSize()); checkErr != nil {
		return nil, xcodes.ErrQuotaExceeded.Wrap(checkErr)
	}

	bucket := conv.ResolveBucket(req.GetBucket(), s.cfg.Storage.DefaultBucket)

	// Optional vendor check: if the caller pinned a vendor, the resolved bucket
	// must belong to it. UNSPECIFIED = skip (legacy behavior).
	if v := req.GetVendor(); v != storagev1.Vendor_VENDOR_UNSPECIFIED {
		if actual := s.registry.VendorForBucket(bucket); actual != v {
			return nil, xcodes.ErrBucketVendorMismatch.New(fmt.Sprintf("bucket %q belongs to %v, not %v", bucket, actual, v))
		}
	}

	// isPublic is derived from the bucket's ACL, not the client request.
	bucketCfg, err := s.registry.BucketConfig(bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	isPublic := isPublicBucketACL(bucketCfg.ACL)

	// Check MD5 dedup: if same file already exists, create UserFile instantly.
	vendor := int32(s.registry.VendorForBucket(bucket))
	existing, found, findErr := dal.FindObjectByVendorBucketMD5(ctx, s.db, vendor, bucket, req.GetMd5())
	if findErr != nil {
		return nil, xcodes.ErrInternal.Wrap(findErr)
	}
	if found {
		fileInfo, txErr := s.handleInstantUpload(ctx, ownerType, ownerID, existing, req.GetFilename(), req.GetFilePath(), req.GetDescription(), req.GetMetadata(), isPublic, req.GetRequestId())
		if txErr != nil {
			return nil, txErr
		}
		return &storagev1.GenerateUploadURLResponse{
			Instant:  true,
			FileId:   fileInfo.Id,
			FileInfo: fileInfo,
		}, nil
	}

	// Not found: generate upload URL. The session/token prelude is shared with
	// GetSTSCredential via prepareUpload — only the final provider-specific
	// step differs (pre-signed PUT vs STS credential). The shared prelude
	// guarantees ConfirmUpload has a SessionID to cross-check, regardless of
	// which issue path the caller took.
	ttl := s.cfg.Storage.UploadTokenTTL
	if ttl == 0 {
		ttl = 30 * time.Minute
	}

	prepared, prepErr := s.prepareUpload(ctx, ownerType, ownerID, bucket, ttl, fileMeta{
		md5:         req.GetMd5(),
		filename:    req.GetFilename(),
		contentType: req.GetContentType(),
		filePath:    req.GetFilePath(),
		description: req.GetDescription(),
		metadata:    req.GetMetadata(),
		size:        req.GetSize(),
		requestID:   req.GetRequestId(),
	})
	if prepErr != nil {
		return nil, prepErr
	}
	if prepared.instant {
		return &storagev1.GenerateUploadURLResponse{
			Instant:  true,
			FileId:   prepared.fileID,
			FileInfo: prepared.fileInfo,
		}, nil
	}

	p, err := s.registry.ProviderForBucket(bucket)
	if err != nil {
		return nil, xcodes.ErrProviderNotFound.Wrap(err)
	}

	uploadURL, headers, presignErr := p.PresignPutObject(ctx, bucket, prepared.session.ObjectKey, prepared.resolvedTTL)
	if presignErr != nil {
		return nil, fmt.Errorf("presign put object: %w", presignErr)
	}

	respHeaders := make(map[string]string)
	for k, v := range headers {
		if len(v) > 0 {
			respHeaders[k] = v[0]
		}
	}

	return &storagev1.GenerateUploadURLResponse{
		Instant:     false,
		UploadToken: prepared.token,
		UploadUrl:   uploadURL,
		ObjectKey:   prepared.session.ObjectKey,
		Headers:     respHeaders,
	}, nil
}

// ConfirmUpload verifies and finalizes an upload. Idempotent on session:
// if the session has already been confirmed, the previously-created File is
// returned without re-reserving quota or re-contacting the object store.
// No rate limit: the upload URL was already rate-limited at issue time;
// confirm only creates the DB record.
func (s *Service) ConfirmUpload(ctx context.Context, req *storagev1.ConfirmUploadRequest) (*storagev1.ConfirmUploadResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	token, err := verifyUploadToken(req.GetUploadToken(), s.cfg.Storage.UploadTokenSecret, ownerID, ownerType)
	if err != nil {
		if isUploadTokenExpired(err) {
			return nil, xcodes.ErrUploadTokenExpired.Wrap(err)
		}
		return nil, xcodes.ErrUploadTokenInvalid.Wrap(err)
	}
	if token.SessionID == 0 {
		// Legacy pre-session token: reject so callers refresh.
		return nil, xcodes.ErrUploadTokenInvalid.New("legacy token without session_id; please fetch a new one")
	}

	session, err := dal.GetUploadSessionByID(ctx, s.db, token.SessionID)
	if err != nil {
		return nil, err
	}
	// Cross-check: token fields must match the session. A mismatch means the
	// token was tampered with or the session row was corrupted. OwnerType is
	// verified by verifyUploadToken above; OwnerID/MD5/Size are verified here
	// against the persisted session row.
	if session.OwnerID != token.OwnerID || session.OwnerType != token.OwnerType || session.MD5 != token.MD5 || session.Size != token.Size {
		return nil, xcodes.ErrUploadTokenInvalid.New("session/token mismatch")
	}

	// Idempotent: session already confirmed in a previous ConfirmUpload call.
	// Return the existing File + StorageObject without re-contacting OSS or
	// re-reserving quota.
	//
	// Edge case: if the File was soft-deleted between confirm attempts (e.g.,
	// owner called DeleteFile), GetByID returns ErrFileNotFound. We surface
	// this rather than returning stale data — the caller should fetch a new
	// upload_token if they want to re-upload.
	if session.Status == int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED) {
		if session.FileID == nil {
			return nil, xcodes.ErrInternal.New("confirmed session has no file_id")
		}
		file, err := dal.GetFileByID(ctx, s.db, *session.FileID)
		if err != nil {
			return nil, err
		}
		obj, err := dal.GetObjectByID(ctx, s.db, file.ObjectID)
		if err != nil {
			return nil, err
		}
		return &storagev1.ConfirmUploadResponse{FileId: file.ID, FileInfo: buildUserFileInfo(file, obj)}, nil
	}
	if session.Status != int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING) {
		return nil, xcodes.ErrUploadSessionExpired.New()
	}

	// Detect registry drift: vendor is pinned in the session at issue time. If
	// the bucket has since been re-assigned to a different provider, the
	// session no longer reflects reality — reject so the caller fetches a new
	// upload_token instead of writing into the wrong backend.
	if currentVendor := int32(s.registry.VendorForBucket(session.Bucket)); currentVendor != session.Vendor {
		return nil, xcodes.ErrBucketVendorMismatch.New(fmt.Sprintf("bucket %q vendor drifted from %d to %d", session.Bucket, session.Vendor, currentVendor))
	}

	// Re-derive IsPublic from the bucket ACL at confirm time. The session row
	// carries the issue-time value too, but reading the live config avoids
	// stale state if the bucket's ACL changed between issue and confirm.
	confirmBucketCfg, err := s.registry.BucketConfig(session.Bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	confirmIsPublic := isPublicBucketACL(confirmBucketCfg.ACL)

	p, err := s.registry.ProviderForBucket(session.Bucket)
	if err != nil {
		return nil, xcodes.ErrProviderNotFound.Wrap(err)
	}

	// Verify file exists in cloud storage. Object key is sourced from the
	// session (created with the resolved key at issue time).
	info, err := p.HeadObject(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	// Verify MD5 checksum matches what was declared in the session.
	actualETag := strings.Trim(info.ETag, "\"")
	if actualETag != "" && actualETag != session.MD5 {
		return nil, xcodes.ErrMD5Mismatch.New()
	}

	// Verify actual file size matches declared size.
	if info.Size != session.Size {
		return nil, xcodes.ErrFileSizeExceeded.New(fmt.Sprintf("declared %d bytes, actual %d bytes", session.Size, info.Size))
	}

	// Verify ContentType matches what was declared in the session. Empty
	// session.ContentType covers callers that legitimately don't know the type
	// ahead of time; empty info.ContentType covers providers that didn't
	// surface it (notably S3 HeadObject omits Content-Type on multipart
	// uploads). Only fail when both sides are populated and disagree — a
	// mismatch means the client uploaded bytes that the cloud saw as a
	// different type than what we'll record on the StorageObject, which would
	// mis-serve the file later.
	if session.ContentType != "" && info.ContentType != "" &&
		!strings.EqualFold(session.ContentType, info.ContentType) {
		return nil, xcodes.ErrContentTypeMismatch.New(fmt.Sprintf(
			"declared %q, actual %q", session.ContentType, info.ContentType))
	}

	// Verify ObjectACL is consistent with the session's privacy intent. When
	// the session is private (IsPublic=false) but the cloud reports the object
	// as public-read or public-read-write, the upload bypassed our policy —
	// reject rather than persist a publicly-readable StorageObject. Empty ACL
	// (provider didn't surface it) is allowed: we can't verify, and S3 will
	// almost always hit this branch. "default" is also allowed since it means
	// the object inherits the bucket default, which we trust at config time.
	if !confirmIsPublic && isPublicACL(info.ObjectACL) {
		return nil, xcodes.ErrObjectACLViolation.New(fmt.Sprintf(
			"session is private but object has ACL %q", info.ObjectACL))
	}

	obj := &models.StorageObject{
		Vendor:       session.Vendor,
		Bucket:       session.Bucket,
		ObjectKey:    session.ObjectKey,
		MD5:          session.MD5,
		Size:         info.Size,
		ContentType:  session.ContentType,
		ETag:         info.ETag,
		StorageClass: int32(storagev1.StorageClass_STORAGE_CLASS_STANDARD),
		IsPublic:     confirmIsPublic,
	}
	if obj.ID, err = s.gid.NextID(ctx); err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "generate object id")
	}

	var result *storagev1.ConfirmUploadResponse
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		createdObj, inserted, err := dal.CreateOrGetObject(ctx, tx, obj)
		if err != nil {
			return err
		}

		uf := &models.StorageFile{
			OwnerType:   ownerType,
			OwnerID:     ownerID,
			ObjectID:    createdObj.ID,
			Filename:    session.Filename,
			FilePath:    session.FilePath,
			Description: session.Description,
			Metadata:    session.Metadata,
			// Mirror the object's IsPublic (derived from bucket ACL) so the
			// file can be queried without joining the object.
			IsPublic: createdObj.IsPublic,
		}
		id, gidErr := s.gid.NextID(ctx)
		if gidErr != nil {
			return xcodes.ErrInternal.Wrapf(gidErr, "generate file id")
		}
		uf.ID = id
		if createErr := dal.CreateFile(ctx, tx, uf); createErr != nil {
			return createErr
		}

		if !inserted {
			if refErr := dal.IncrObjectRefCount(ctx, tx, createdObj.ID); refErr != nil {
				return refErr
			}
		}

		if reserveErr := s.host.Reserve(ctx, tx, ownerType, ownerID, createdObj.Size); reserveErr != nil {
			return reserveErr
		}

		// Atomically transition the session PENDING → CONFIRMED. Concurrent
		// confirms race here: the loser's MarkConfirmed returns 0 rows
		// (ErrUploadSessionNotPending), causing this whole transaction to
		// roll back (the file create is undone) and the caller to retry into
		// the idempotent branch above.
		if markErr := dal.MarkUploadSessionConfirmed(ctx, tx, session.ID, uf.ID); markErr != nil {
			return markErr
		}

		result = &storagev1.ConfirmUploadResponse{
			FileId:   uf.ID,
			FileInfo: buildUserFileInfo(uf, createdObj),
		}
		return nil
	})
	if txErr != nil {
		s.host.RecordOutcome(ctx, AuditEvent{
			Action:     storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_CONFIRM,
			RequestID:  req.GetRequestId(),
			OwnerType:  ownerType,
			OwnerID:    ownerID,
			TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
			TargetID:   session.ID,
		}, txErr)
		return nil, fmt.Errorf("confirm upload transaction: %w", txErr)
	}

	s.host.RecordOutcome(ctx, AuditEvent{
		Action:     storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_CONFIRM,
		RequestID:  req.GetRequestId(),
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   result.FileId,
		After: conv.MustToMap(FileSnapshot{
			Filename:    session.Filename,
			FilePath:    session.FilePath,
			Description: session.Description,
			Size:        info.Size,
			ContentType: session.ContentType,
			MD5:         session.MD5,
			IsPublic:    confirmIsPublic,
		}),
	}, nil)

	return result, nil
}

// GetSTSCredential returns a short-lived STS credential scoped to a single
// object key, for direct-to-provider uploads where a pre-signed URL is not
// supported (or where the client prefers STS). Enforces the same per-owner
// upload rate limit as GenerateUploadURL.
func (s *Service) GetSTSCredential(ctx context.Context, req *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	if err := s.checkUploadRateLimit(ctx, ownerType, ownerID); err != nil {
		return nil, err
	}

	bucket := conv.ResolveBucket(req.GetBucket(), s.cfg.Storage.DefaultBucket)
	if v := req.GetVendor(); v != storagev1.Vendor_VENDOR_UNSPECIFIED {
		if actual := s.registry.VendorForBucket(bucket); actual != v {
			return nil, xcodes.ErrBucketVendorMismatch.New(fmt.Sprintf("bucket %q belongs to %v, not %v", bucket, actual, v))
		}
	}

	// Fail-fast: if allowed_extensions is set and filename's extension is not
	// in the list, reject before any STS call. Saves AssumeRole quota (Aliyun
	// rate-limits AssumeRole) and gives the caller an immediate, specific error
	// instead of an opaque OSS rejection at PUT time.
	allowed := normalizeExtensions(req.GetAllowedExtensions())
	if err := validateFilenameExtension(req.GetFilename(), allowed); err != nil {
		return nil, err
	}

	ttl := req.GetTtl().AsDuration()
	file := fileMeta{
		md5:         req.GetMd5(),
		size:        req.GetMaxSize(),
		contentType: req.GetContentType(),
		filename:    req.GetFilename(),
		filePath:    req.GetFilePath(),
		description: req.GetDescription(),
		metadata:    req.GetMetadata(),
		requestID:   req.GetRequestId(),
	}

	result, err := s.issueUploadCredential(ctx, ownerType, ownerID, bucket, ttl, file, allowed)
	if err != nil {
		return nil, err
	}

	if result.instant {
		return &storagev1.GetSTSCredentialResponse{Instant: true, FileId: result.fileID, FileInfo: result.fileInfo}, nil
	}
	return &storagev1.GetSTSCredentialResponse{
		UploadToken:   result.uploadToken,
		AccessKey:     result.accessKey,
		SecretKey:     result.secretKey,
		SecurityToken: result.securityToken,
		Endpoint:      result.endpoint,
		Bucket:        result.bucket,
		ObjectKey:     result.objectKey,
		ExpiresAt:     result.expiresAt,
	}, nil
}

// prepareUpload runs the per-file prelude shared by the pre-signed URL and
// STS credential flows (rate limit + bucket resolution done by the caller):
//  1. MD5 dedup → instant File (early return)
//  2. checkQuota
//  3. session dedup + create (under Redis lock)
//  4. sign upload_token with session_id
//
// Returns either an instant File or the signed token + the session + bucket
// config the caller needs to finish its provider-specific step (Presign or STS).
func (s *Service) prepareUpload(ctx context.Context, ownerType int32, ownerID int64, bucket string, ttl time.Duration, file fileMeta) (*prepareResult, error) {
	// Resolve ttl once so the session expiry, upload_token expiry, and any
	// subsequent credential (STS / presigned URL) all share the same value.
	// sts would otherwise silently substitute its default for ttl=0, leaving
	// the session expired at birth.
	ttl = s.sts.ResolveTTL(ttl)

	vendor := int32(s.registry.VendorForBucket(bucket))

	// 1. MD5 dedup
	existing, found, err := dal.FindObjectByVendorBucketMD5(ctx, s.db, vendor, bucket, file.md5)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	bucketCfg, err := s.registry.BucketConfig(bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	isPublic := isPublicBucketACL(bucketCfg.ACL)

	if found {
		fileInfo, txErr := s.handleInstantUpload(ctx, ownerType, ownerID, existing, file.filename, file.filePath, file.description, file.metadata, isPublic, file.requestID)
		if txErr != nil {
			return nil, txErr
		}
		return &prepareResult{instant: true, fileID: fileInfo.Id, fileInfo: fileInfo}, nil
	}

	objectKey := conv.ObjectKeyFromMD5(bucketCfg.KeyPrefix, file.md5)

	// 2. checkQuota (read-only; no DB write)
	if checkErr := s.host.CheckQuota(ctx, s.db, ownerType, ownerID, file.size); checkErr != nil {
		return nil, xcodes.ErrQuotaExceeded.Wrap(checkErr)
	}

	// 3. session dedup + create
	session, err := s.findOrCreateSession(ctx, ownerType, ownerID, vendor, bucket, objectKey, file, ttl)
	if err != nil {
		return nil, err
	}

	// 4. sign upload_token with session_id. IsPublic is intentionally NOT
	// pinned in the token — ConfirmUpload re-derives it from the live bucket
	// ACL via session.Bucket, so a bucket ACL change between issue and confirm
	// is reflected in the persisted object.
	token := &uploadToken{
		SessionID:   session.ID,
		OwnerID:     ownerID,
		OwnerType:   ownerType,
		MD5:         file.md5,
		Size:        file.size,
		ContentType: file.contentType,
		Bucket:      bucket,
		Vendor:      vendor,
		Filename:    file.filename,
		FilePath:    file.filePath,
		Description: file.description,
		Metadata:    file.metadata,
		ExpiresAt:   time.Now().Add(ttl).Unix(),
	}
	tokenStr, err := signUploadToken(token, s.cfg.Storage.UploadTokenSecret)
	if err != nil {
		return nil, xcodes.ErrUploadTokenInvalid.Wrap(err)
	}

	return &prepareResult{
		instant:     false,
		token:       tokenStr,
		session:     session,
		bucketCfg:   bucketCfg,
		vendor:      vendor,
		resolvedTTL: ttl,
	}, nil
}

// prepareResult holds either an instant File (MD5 dedup hit) or the signed
// token + session + bucket config the caller needs to finish its provider-
// specific step (PresignPutObject or STS).
type prepareResult struct {
	instant     bool
	fileID      int64
	fileInfo    *storagev1.UserFileInfo
	token       string
	session     *models.StorageUploadSession
	bucketCfg   *config.BucketConfig
	vendor      int32
	resolvedTTL time.Duration
}

// issueUploadCredential runs the STS credential flow:
//
//	1-4. shared prepareUpload prelude (dedup/quota/session/token)
//	5. STS credential (cached per owner+vendor+bucket)
func (s *Service) issueUploadCredential(ctx context.Context, ownerType int32, ownerID int64, bucket string, ttl time.Duration, file fileMeta, allowedExtensions []string) (*issueResult, error) {
	prepared, err := s.prepareUpload(ctx, ownerType, ownerID, bucket, ttl, file)
	if err != nil {
		return nil, err
	}
	if prepared.instant {
		return &issueResult{instant: true, fileID: prepared.fileID, fileInfo: prepared.fileInfo}, nil
	}

	stsPolicy := &storage.STSPolicy{
		OwnerID:           ownerID,
		OwnerType:         ownerType,
		Bucket:            bucket,
		KeyPrefix:         prepared.bucketCfg.KeyPrefix,
		AllowedExtensions: allowedExtensions,
		AllowedActions:    []string{types.PutObjectActionForVendor(prepared.vendor)},
		MaxSize:           file.size,
		TTL:               prepared.resolvedTTL,
	}
	creds, err := s.sts.Get(ctx, ownerType, ownerID, prepared.vendor, bucket, prepared.resolvedTTL, stsPolicy)
	if err != nil {
		return nil, fmt.Errorf("get STS token: %w", err)
	}

	return &issueResult{
		instant:       false,
		uploadToken:   prepared.token,
		accessKey:     creds.AccessKey,
		secretKey:     creds.SecretKey,
		securityToken: creds.SecurityToken,
		endpoint:      creds.Endpoint,
		bucket:        creds.Bucket,
		// Full object key (e.g. "<keyPrefix>/<md5[:2]>/<md5>") — clients PUT
		// here directly with the STS credential. Previously this returned
		// creds.ObjectKeyPrefix (just the keyPrefix), which forced clients
		// to know the sharding rule to construct the full path.
		objectKey: prepared.session.ObjectKey,
		expiresAt: creds.ExpiresAt.Unix(),
	}, nil
}

// findOrCreateSession returns an existing PENDING session for the same
// (owner, md5, size) or creates a new one.
//
// Two-layer defense against duplicate PENDING sessions:
//
//  1. Redis dedup lock (best-effort front layer). Three acquire outcomes:
//     - success: hold the lock for the rest of this call.
//     - redisx.ErrLockFailed (lock contention): fall through — the lock
//     holder will create the session, we'll pick it up via FindPendingDedup.
//     - errDedupLockDisabled (Redis not configured): fall through —
//     this is an intentional deployment mode; the DB unique index is the
//     only backstop.
//     - any other error (Redis unreachable, network error, ctx cancelled):
//     fail closed. Surfacing an internal error is safer than risking
//     duplicate PENDING sessions / quota drift.
//
//  2. DB-level partial unique index idx_upload_sessions_pending_dedup on
//     (owner_type, owner_id, md5, size) scoped to status=PENDING. Even if
//     both layers above let two callers race to Create, the loser's INSERT
//     hits ON CONFLICT DO NOTHING (see UploadSessionRepo.Create) and the
//     caller re-reads via FindPendingDedup.
func (s *Service) findOrCreateSession(ctx context.Context, ownerType int32, ownerID int64, vendor int32, bucket, objectKey string, file fileMeta, ttl time.Duration) (*models.StorageUploadSession, error) {
	if lockID, lockErr := s.dedupLock.acquire(ctx, ownerType, ownerID, file.md5, file.size); lockErr == nil {
		defer s.dedupLock.release(ctx, ownerType, ownerID, file.md5, file.size, lockID)
	} else if !errors.Is(lockErr, redisx.ErrLockFailed) && !errors.Is(lockErr, errDedupLockDisabled) {
		// Fail closed: any non-contention, non-disabled acquire error means
		// Redis is configured but currently unreachable. Better to surface an
		// internal error than to fall through and risk duplicate PENDING
		// sessions / quota drift.
		return nil, xcodes.ErrInternal.Wrapf(lockErr, "acquire upload dedup lock")
	}
	// lockErr == ErrLockFailed (someone else holds the lock; they will create
	// the session) or errDedupLockDisabled (Redis not configured): in
	// both cases we fall through to FindPendingDedup and the DB-level partial
	// unique index is the backstop.

	if existing, found, err := dal.FindPendingUploadSessionDedup(ctx, s.db, ownerType, ownerID, file.md5, file.size); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	} else if found {
		return existing, nil
	}

	// Derive IsPublic from the bucket ACL at session creation time. This
	// snapshot is what ConfirmUpload audits against. If the bucket's ACL
	// changes between issue and confirm, the ACL-verification step in
	// ConfirmUpload will re-derive from live config.
	bucketCfg, err := s.registry.BucketConfig(bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	isPublic := isPublicBucketACL(bucketCfg.ACL)

	id, err := s.gid.NextID(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	session := &models.StorageUploadSession{
		ID:          id,
		OwnerType:   ownerType,
		OwnerID:     ownerID,
		Bucket:      bucket,
		ObjectKey:   objectKey,
		MD5:         file.md5,
		Size:        file.size,
		ContentType: file.contentType,
		Filename:    file.filename,
		FilePath:    file.filePath,
		Description: file.description,
		Metadata:    models.MapJSON(file.metadata),
		IsPublic:    isPublic,
		Vendor:      vendor,
		Status:      int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING),
		ExpiresAt:   time.Now().Add(ttl),
	}
	inserted, err := dal.CreateUploadSession(ctx, s.db, session)
	if err != nil {
		return nil, err
	}
	if !inserted {
		// Lost the race to a concurrent caller (or the lock holder just
		// committed). Re-read to pick up the winner's session.
		existing, found, ferr := dal.FindPendingUploadSessionDedup(ctx, s.db, ownerType, ownerID, file.md5, file.size)
		if ferr != nil {
			return nil, xcodes.ErrInternal.Wrapf(ferr, "find existing after on-conflict")
		}
		if !found {
			// ON CONFLICT DO NOTHING fired but no PENDING row is visible.
			// The most plausible cause is a partial-index mismatch (e.g.
			// a soft-deleted row is hitting the constraint) — surface as
			// internal rather than silently inventing a session.
			return nil, xcodes.ErrInternal.New("upload session not found after on-conflict do nothing")
		}
		return existing, nil
	}

	s.host.RecordOutcome(ctx, AuditEvent{
		Action:     storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_CREATE,
		RequestID:  file.requestID,
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   session.ID,
		After: conv.MustToMap(SessionSnapshot{
			ID:        session.ID,
			OwnerType: session.OwnerType,
			OwnerID:   session.OwnerID,
			Vendor:    session.Vendor,
			Bucket:    session.Bucket,
			ObjectKey: session.ObjectKey,
			MD5:       session.MD5,
			Size:      session.Size,
			Status:    session.Status,
			ExpiresAt: session.ExpiresAt.Format(time.RFC3339),
		}),
	}, nil)

	return session, nil
}

// handleInstantUpload performs the instant upload (MD5 dedup hit) inside a transaction.
// requestID is forwarded to the audit log; pass the caller's req.GetRequestId().
func (s *Service) handleInstantUpload(ctx context.Context, ownerType int32, ownerID int64, existing *models.StorageObject, filename, filePath, description string, metadata map[string]string, isPublic bool, requestID string) (*storagev1.UserFileInfo, error) {
	if checkErr := s.host.CheckQuota(ctx, s.db, ownerType, ownerID, existing.Size); checkErr != nil {
		return nil, xcodes.ErrQuotaExceeded.Wrap(checkErr)
	}

	var fileInfo *storagev1.UserFileInfo
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		uf := &models.StorageFile{
			OwnerType:   ownerType,
			OwnerID:     ownerID,
			ObjectID:    existing.ID,
			Filename:    filename,
			FilePath:    filePath,
			Description: description,
			Metadata:    models.MapJSON(metadata),
			IsPublic:    isPublic,
		}
		if id, gidErr := s.gid.NextID(ctx); gidErr != nil {
			return fmt.Errorf("generate file id: %w", gidErr)
		} else {
			uf.ID = id
		}
		if createErr := dal.CreateFile(ctx, tx, uf); createErr != nil {
			return createErr
		}

		if refErr := dal.IncrObjectRefCount(ctx, tx, existing.ID); refErr != nil {
			return refErr
		}

		if reserveErr := s.host.Reserve(ctx, tx, ownerType, ownerID, existing.Size); reserveErr != nil {
			return reserveErr
		}

		fileInfo = buildUserFileInfo(uf, existing)
		return nil
	})
	if txErr != nil {
		return nil, fmt.Errorf("instant upload transaction: %w", txErr)
	}

	s.host.RecordOutcome(ctx, AuditEvent{
		Action:     storagev1.AuditAction_AUDIT_ACTION_UPLOAD,
		RequestID:  requestID,
		OwnerType:  ownerType,
		OwnerID:    ownerID,
		TargetType: storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_FILE,
		TargetID:   fileInfo.Id,
		After: conv.MustToMap(FileSnapshot{
			Filename:    filename,
			FilePath:    filePath,
			Description: description,
			Size:        existing.Size,
			IsPublic:    isPublic,
		}),
	}, nil)

	return fileInfo, nil
}

func (s *Service) checkUploadRateLimit(ctx context.Context, ownerType int32, ownerID int64) error {
	if s.limiter == nil {
		return nil
	}
	purpose := "upload:" + strconv.FormatInt(int64(ownerType), 10)
	allowed, err := s.limiter.Allow(ctx, purpose, strconv.FormatInt(ownerID, 10))
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	if !allowed {
		return xcodes.ErrRateLimited.New()
	}
	return nil
}

// --- internal helpers ---

// isPublicBucketACL reports whether the bucket ACL denotes a publicly-readable
// bucket. This is the single source of truth for deriving IsPublic at upload
// time — both session creation and object creation go through this helper.
// Keep the constant in sync with internal/service/conv/conv.go.
func isPublicBucketACL(acl string) bool {
	return strings.EqualFold(acl, "public_read") ||
		strings.EqualFold(acl, "public_read_write")
}

// isPublicACL reports whether acl denotes a publicly-readable canned ACL.
// Used by ConfirmUpload to reject uploads that bypassed the session's privacy
// intent. Empty and "default" (Aliyun's "inherit bucket") return false — both
// are treated as "no public grant", with empty meaning "provider didn't
// surface an ACL, can't verify".
func isPublicACL(acl string) bool {
	switch strings.ToLower(acl) {
	case types.ObjectACLPublicRead, types.ObjectACLPublicReadWrite:
		return true
	}
	return false
}

// normalizeExtensions lowercases, trims whitespace, and filters empty entries.
// Service-layer normalization ensures consistent comparison against filename
// extensions (case-insensitive) and consistent Resource wildcards in STS
// policy (Aliyun matches case-sensitively, so we normalize before sending).
//
// Does NOT add a leading '.' — that's the caller's responsibility and
// validated at policy-build time (buildAliyunPolicy rejects strings missing
// the dot).
func normalizeExtensions(exts []string) []string {
	out := make([]string, 0, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// validateFilenameExtension implements the fail-fast check shared by the
// single-file and batch STS flows: when allowed is non-empty and filename is
// non-empty, filename's extension must appear in allowed (case-insensitive).
// Returns nil when the check passes or is skipped (no list / no filename),
// and xcodes.ErrBadRequest when the extension is rejected. Called before any
// STS issuer call so cloud-side AssumeRole quota is not burned on a request
// the cloud would refuse at PUT time anyway.
func validateFilenameExtension(filename string, allowed []string) error {
	if len(allowed) == 0 || filename == "" {
		return nil
	}
	fileExt := strings.ToLower(filepath.Ext(filename))
	for _, a := range allowed {
		if a == fileExt {
			return nil
		}
	}
	return xcodes.ErrBadRequest.New(fmt.Sprintf(
		"filename %q extension %q not in allowed_extensions %v",
		filename, fileExt, allowed))
}

// DedupLock prevents thundering herd of concurrent GetSTSCredential for
// the same (owner, md5, size) from creating duplicate PENDING sessions. Lock
// params come from config.Storage.UploadSession.DedupLock; the underlying
// *redisx.Lock is constructed once at service init and shared by acquire/release.
//
// When Redis is not configured (lock == nil), acquire returns
// NewDedupLock builds the dedup lock. Returns a zero-value DedupLock
// (lock == nil) when rdb is nil — callers fall through to FindPendingDedup.
// cfg fields with zero values fall back to safe defaults so this constructor
// never fails for callers that bypass configx (e.g. tests).
func NewDedupLock(rdb *redis.Client, cfg *config.LockConfig) DedupLock {
	if rdb == nil {
		return DedupLock{}
	}
	if cfg == nil {
		cfg = &config.LockConfig{}
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "upload:dedup"
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 10 * time.Second
	}
	if cfg.Tries <= 0 {
		cfg.Tries = 3
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 100 * time.Millisecond
	}
	lock, err := redisx.NewLock(rdb, &redisx.LockConfig{
		Prefix: cfg.Prefix,
		TTL:    cfg.TTL,
		Tries:  cfg.Tries,
		Wait:   cfg.Wait,
	})
	if err != nil {
		// Defensive: NewLock only fails on empty prefix or non-positive TTL,
		// both handled above. If we ever reach here it's a programmer error.
		panic(fmt.Sprintf("upload: build dedup lock: %v", err))
	}
	return DedupLock{lock: lock}
}

func (l DedupLock) acquire(ctx context.Context, ownerType int32, ownerID int64, md5 string, size int64) (string, error) {
	if l.lock == nil {
		return "", errDedupLockDisabled
	}
	target := fmt.Sprintf("%d:%d:%s:%d", ownerType, ownerID, md5, size)
	return l.lock.Acquire(ctx, target)
}

func (l DedupLock) release(ctx context.Context, ownerType int32, ownerID int64, md5 string, size int64, id string) {
	if l.lock == nil {
		return
	}
	target := fmt.Sprintf("%d:%d:%s:%d", ownerType, ownerID, md5, size)
	_ = l.lock.Release(ctx, target, id)
}

// --- test-only accessors ---
//
// The following accessors exist solely so the parent package's integration tests
// (internal/service, package service) can decode upload tokens and swap the STS
// cache when exercising the upload RPCs end-to-end through the StorageService
// facade. They are NOT part of the supported public API and must not be used by
// production callers. They live in a non-_test file because Go compiles _test
// files only into their own package's test binary, so cross-package test access
// requires a regular file. Exported names carry the "ForTest" suffix to make
// any off-label use obvious in review.

// VerifyTokenForTest decodes and verifies a signed upload token. Test-only.
func VerifyTokenForTest(encoded, secret string, expectedOwnerID int64, expectedOwnerType int32) (*uploadToken, error) {
	return verifyUploadToken(encoded, secret, expectedOwnerID, expectedOwnerType)
}

// SignTokenForTest signs an upload token. Test-only.
func SignTokenForTest(token *uploadToken, secret string) (string, error) {
	return signUploadToken(token, secret)
}

// TokenForTest returns a pointer to a fresh, zero-value upload token that
// the caller can populate field by field. Test-only.
func TokenForTest() *uploadToken { return &uploadToken{} }

// SetSTS swaps the internal STS service. Test-only.
func SetSTS(s *Service, rdb *redis.Client, issuer sts.Issuer, cfg *config.STSConfig) {
	s.sts = sts.New(rdb, issuer, cfg)
}
