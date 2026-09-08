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
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/storage-service/internal/appauth"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/service/sts"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"
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
	gid      gidservice.Service
	cfg      *config.Config
	limiter  ratelimit.Limiter

	sts  *sts.Service
	lock *redisx.Lock // shared by all upload lock domains (dedup / object / reap); nil when Redis is unavailable
	host Host
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

// Lock target domains. All upload locks share one *redisx.Lock instance
// (Service.lock), distinguished by target key with a single configurable
// prefix (see NewLock) — there is no per-domain lock struct.
const (
	lockTargetDedup  = "dedup"  // session-dedup target suffix: <ownerType>:<ownerID>:<md5>:<size>
	lockTargetObject = "object" // object-dedup target suffix: <vendor>:<bucket>:<md5>
	lockTargetReap   = "reap"   // cross-replica GC exclusion target (single key)
)

// NewLock builds the single *redisx.Lock shared by all upload lock domains
// (session dedup, object dedup, GC reap). The prefix and TTL/Tries/Wait come
// from cfg (config.Storage.UploadSession.Lock), so operators customize the
// Redis key namespace and tuning in one place. Returns nil when Redis is
// unavailable — callers then run lock-free (dedup best-effort; GC relies on the
// per-row CAS in MarkUploadSessionExpired for correctness).
func NewLock(rdb *redis.Client, cfg *config.LockConfig) *redisx.Lock {
	if rdb == nil {
		return nil
	}
	if cfg == nil {
		cfg = &config.LockConfig{}
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "upload"
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
		// both handled above. Reaching here is a programmer error.
		panic(fmt.Sprintf("upload: build lock: %v", err))
	}
	return lock
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB       *gorm.DB
	Registry *storage.Registry
	GID      gidservice.Service
	Cfg      *config.Config
	Limiter  ratelimit.Limiter
	Redis    *redis.Client
	STS      *config.STSConfig
	Lock     *redisx.Lock
	Host     Host
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

// New constructs an upload.Service from injected deps. The STS cache is built
// internally (it adapts the registry, which is upload-domain state); the single
// shared *redisx.Lock is injected so the parent controls its config (prefix,
// TTL, retries) in one place.
func New(d *Deps) *Service {
	issuer := sts.FuncIssuer(func(ctx context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
		p, err := d.Registry.ProviderForBucket(policy.Bucket)
		if err != nil {
			return nil, err
		}
		return p.GetSTSToken(ctx, policy)
	})
	return &Service{
		db:       d.DB,
		registry: d.Registry,
		gid:      d.GID,
		cfg:      d.Cfg,
		limiter:  d.Limiter,
		sts:      sts.New(d.Redis, issuer, d.STS),
		lock:     d.Lock,
		host:     d.Host,
	}
}

// authenticateApp resolves the calling app from x-app-key/x-app-secret
// metadata against the registry snapshot. Mirrors message-service: the check
// runs in the service layer (not an interceptor) so module-mode in-process
// callers share the exact same path. Fail-closed on missing credentials,
// unknown/disabled app, or secret mismatch.
func (s *Service) authenticateApp(ctx context.Context) (*models.StorageApp, error) {
	appKey, appSecret, ok := appauth.Credentials(ctx)
	if !ok {
		return nil, xcodes.ErrAppUnauthorized.New("missing app credentials")
	}
	app := s.registry.App(appKey)
	if app == nil || app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("app %q not found or disabled", appKey))
	}
	if app.AppSecret != appSecret {
		return nil, xcodes.ErrAppUnauthorized.New("bad app credentials")
	}
	return app, nil
}

// appBucket maps the app's bucket binding + requested audience class to a
// bucket name (see conv.ResolveAppBucket).
func (s *Service) appBucket(app *models.StorageApp, visibility storagev1.Visibility) (string, error) {
	return conv.ResolveAppBucket(
		s.registry.BucketNameByID(app.BucketID),
		s.registry.DefaultBucket(), s.registry.PublicBucket(), visibility)
}

// GenerateUploadURL reserves quota, creates an upload session row, and returns
// either a direct upload URL or STS credentials (depending on provider) so the
// client can push the object to object storage. Enforces per-owner upload rate
// limits.
func (s *Service) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	app, err := s.authenticateApp(ctx)
	if err != nil {
		return nil, err
	}
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	if err := s.checkUploadRateLimit(ctx, ownerType, ownerID); err != nil {
		return nil, err
	}

	if checkErr := s.host.CheckQuota(ctx, s.db, ownerType, ownerID, req.GetSize()); checkErr != nil {
		return nil, xcodes.ErrQuotaExceeded.Wrap(checkErr)
	}

	bucket, err := s.appBucket(app, req.GetVisibility())
	if err != nil {
		return nil, err
	}

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

	// Check dedup within the app's namespace: the content-addressed key embeds
	// the app key_prefix, so an identical file under this prefix returns an
	// instant File, while another app's copy stays private to it.
	vendor := int32(s.registry.VendorForBucket(bucket))
	dedupKey := conv.ObjectKeyFromMD5(app.KeyPrefix, req.GetMd5())
	existing, found, findErr := dal.FindObjectByVendorBucketObjectKey(ctx, s.db, vendor, bucket, dedupKey)
	if findErr != nil {
		return nil, xcodes.ErrInternal.Wrap(findErr)
	}
	if found {
		fileInfo, txErr := s.handleInstantUpload(ctx, app.AppKey, ownerType, ownerID, existing, req.GetFilename(), req.GetFilePath(), req.GetDescription(), req.GetMetadata(), isPublic, req.GetRequestId())
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

	prepared, prepErr := s.prepareUpload(ctx, app, ownerType, ownerID, bucket, ttl, fileMeta{
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
	app, err := s.authenticateApp(ctx)
	if err != nil {
		return nil, err
	}
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
	// The issuing app settles its own sessions — one app can never confirm
	// another app's in-flight upload. Pre-app tokens carry an empty AppKey
	// and are settled by any app (one deploy's worth of history).
	if token.AppKey != "" && token.AppKey != app.AppKey {
		return nil, xcodes.ErrUploadTokenInvalid.New("token was issued to a different app")
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
	if session.AppKey != "" && session.AppKey != token.AppKey {
		return nil, xcodes.ErrUploadTokenInvalid.New("session/token app mismatch")
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
	// (provider didn't surface it) is allowed: we can't verify what the cloud
	// didn't report. "default" is also allowed since it means the object
	// inherits the bucket default, which we trust at config time. The S3
	// provider fills ObjectACL via GetObjectAcl, so this check is live there;
	// the STS hardening flags (LockObjectACL/DenyPutObjectACL) prevent the
	// violation from occurring in the first place on STS uploads.
	if !confirmIsPublic && isPublicACL(info.ObjectACL) {
		return nil, xcodes.ErrObjectACLViolation.New(fmt.Sprintf(
			"session is private but object has ACL %q", info.ObjectACL))
	}

	// Two-phase landing: the client wrote to the unguessable staging key
	// (session.ObjectKey). Derive the content-addressed final key from the
	// issuing app's prefix (snapshotted on the session) and move the verified
	// bytes there server-side — clients never hold a credential for a final
	// key, so confirmed objects cannot be overwritten through the upload path.
	// Pre-app sessions carry an empty prefix; for them the derivation equals
	// the legacy unprefixed key. Sessions minted before the two-phase flow
	// carry the final key directly; for those the copy is a no-op.
	finalKey := conv.ObjectKeyFromMD5(session.KeyPrefix, session.MD5)
	finalInfo := info
	if session.ObjectKey != finalKey {
		finalInfo, err = ensureFinalObject(ctx, p, session.Bucket, session.ObjectKey, finalKey, session.MD5)
		if err != nil {
			return nil, err
		}
	}

	obj := &models.StorageObject{
		Vendor:       session.Vendor,
		Bucket:       session.Bucket,
		ObjectKey:    finalKey,
		MD5:          session.MD5,
		Size:         finalInfo.Size,
		ContentType:  session.ContentType,
		ETag:         finalInfo.ETag,
		StorageClass: int32(storagev1.StorageClass_STORAGE_CLASS_STANDARD),
		IsPublic:     confirmIsPublic,
	}
	if obj.ID, err = gidservice.NextID(ctx, s.gid); err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "generate object id")
	}

	// Object dedup: serialize concurrent confirms of identical content so
	// CreateOrGetObject's check-then-insert does not create duplicate object
	// rows. Best-effort like the session dedup lock — contention falls through
	// (rare duplicate accepted for DB portability); Redis errors fail closed.
	if s.lock != nil {
		target := objectLockTarget(session.Vendor, session.Bucket, session.MD5)
		lockID, lockErr := s.lock.Acquire(ctx, target)
		switch {
		case lockErr == nil:
			defer func() { _ = s.lock.Release(context.Background(), target, lockID) }()
		case errors.Is(lockErr, redisx.ErrLockFailed):
			// Another confirm of the same content is mid-flight; fall through.
			// CreateOrGetObject may create a duplicate — accepted tradeoff.
		default:
			return nil, xcodes.ErrInternal.Wrapf(lockErr, "acquire object dedup lock")
		}
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
			AppKey:      session.AppKey,
			Filename:    session.Filename,
			FilePath:    session.FilePath,
			Description: session.Description,
			Metadata:    session.Metadata,
			// Mirror the object's IsPublic (derived from bucket ACL) so the
			// file can be queried without joining the object.
			IsPublic: createdObj.IsPublic,
		}
		id, gidErr := gidservice.NextID(ctx, s.gid)
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
			Size:        finalInfo.Size,
			ContentType: session.ContentType,
			MD5:         session.MD5,
			IsPublic:    confirmIsPublic,
		}),
	}, nil)

	// Reclaim the staging object now that the final key holds the bytes.
	// Best-effort: on failure the settled-session GC sweep (reap.go) deletes it
	// after the session expires, so a transient delete error cannot leak the
	// bytes permanently. Runs after the commit — deleting before it would race
	// a rollback (the bytes would still be wanted at the staging key).
	if session.ObjectKey != finalKey {
		if delErr := p.DeleteObject(ctx, session.Bucket, session.ObjectKey); delErr != nil {
			slog.Warn("confirm upload: reclaim staging object", "session_id", session.ID,
				"key", session.ObjectKey, "error", delErr)
		}
	}

	return result, nil
}

// ensureFinalObject lands the verified staging bytes at the content-addressed
// final key and returns the final object's Head info.
//
// Global dedup is preserved: when the final key already holds an object whose
// ETag matches the MD5, the copy is skipped and the existing bytes stay
// authoritative. An existing object whose ETag disagrees with its own content
// address is corrupt by definition (legacy pollution or multipart-ETag shape)
// — the copy overwrites it with freshly verified bytes, which heals rather
// than destroys. Note the single-shot CopyObject API caps at 5GB on most
// providers; the enforced MaxUploadBytes ceiling (default 1GB) keeps uploads
// within that limit.
func ensureFinalObject(ctx context.Context, p storage.Provider, bucket, stagingKey, finalKey, md5 string) (*types.ObjectInfo, error) {
	existing, err := p.HeadObject(ctx, bucket, finalKey)
	switch {
	case err == nil:
		if etag := strings.Trim(existing.ETag, "\""); etag != "" && etag == md5 {
			return existing, nil
		}
		// ETag mismatch: fall through and overwrite via copy so the key holds
		// verified bytes for its own content address.
	case errors.Is(err, storage.ErrObjectNotFound):
		// Final object absent — copy below.
	default:
		return nil, xcodes.ErrInternal.Wrapf(err, "head final object %q", finalKey)
	}

	if copyErr := p.CopyObject(ctx, bucket, stagingKey, finalKey); copyErr != nil {
		return nil, xcodes.ErrInternal.Wrapf(copyErr, "copy staging object %q to final key %q", stagingKey, finalKey)
	}
	finalInfo, err := p.HeadObject(ctx, bucket, finalKey)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "verify copied object at final key %q", finalKey)
	}
	return finalInfo, nil
}

// GetSTSCredential returns a short-lived STS credential scoped to a single
// object key, for direct-to-provider uploads where a pre-signed URL is not
// supported (or where the client prefers STS). Enforces the same per-owner
// upload rate limit as GenerateUploadURL.
func (s *Service) GetSTSCredential(ctx context.Context, req *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	app, err := s.authenticateApp(ctx)
	if err != nil {
		return nil, err
	}
	ownerType := int32(req.GetOwner().GetOwnerType())
	ownerID := req.GetOwner().GetOwnerId()

	if err := s.checkUploadRateLimit(ctx, ownerType, ownerID); err != nil {
		return nil, err
	}

	bucket, err := s.appBucket(app, req.GetVisibility())
	if err != nil {
		return nil, err
	}
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

	result, err := s.issueUploadCredential(ctx, app, ownerType, ownerID, bucket, ttl, file)
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
func (s *Service) prepareUpload(ctx context.Context, app *models.StorageApp, ownerType int32, ownerID int64, bucket string, ttl time.Duration, file fileMeta) (*prepareResult, error) {
	// Resolve ttl once so the session expiry, upload_token expiry, and any
	// subsequent credential (STS / presigned URL) all share the same value.
	// sts would otherwise silently substitute its default for ttl=0, leaving
	// the session expired at birth.
	ttl = s.sts.ResolveTTL(ttl)

	// Hard single-file ceiling, enforced before any dedup/quota work on both
	// the instant and credential paths. Inline uploads above this belong on
	// the link (object-storage reference) channel instead.
	if max := s.cfg.Storage.MaxUploadBytes; max > 0 && file.size > max {
		return nil, xcodes.ErrFileSizeExceeded.New(fmt.Sprintf("file size %d exceeds upload limit %d", file.size, max))
	}

	vendor := int32(s.registry.VendorForBucket(bucket))

	// 1. MD5 dedup within the app's namespace (object_key embeds the prefix).
	dedupKey := conv.ObjectKeyFromMD5(app.KeyPrefix, file.md5)
	existing, found, err := dal.FindObjectByVendorBucketObjectKey(ctx, s.db, vendor, bucket, dedupKey)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	bucketCfg, err := s.registry.BucketConfig(bucket)
	if err != nil {
		return nil, xcodes.ErrBucketNotFound.Wrap(err)
	}
	isPublic := isPublicBucketACL(bucketCfg.ACL)

	if found {
		fileInfo, txErr := s.handleInstantUpload(ctx, app.AppKey, ownerType, ownerID, existing, file.filename, file.filePath, file.description, file.metadata, isPublic, file.requestID)
		if txErr != nil {
			return nil, txErr
		}
		return &prepareResult{instant: true, fileID: fileInfo.Id, fileInfo: fileInfo}, nil
	}

	// Two-phase upload: stage the client's PUT at an unguessable temp key.
	// The content-addressed final key (ObjectKeyFromMD5) is derivable from the
	// MD5 alone, so a credential for it would let any caller overwrite an
	// already-confirmed object with the same hash. ConfirmUpload verifies the
	// staged bytes and copies them to the final key server-side.
	objectKey, keyErr := conv.NewTempObjectKey(app.KeyPrefix, ownerType, ownerID, file.md5)
	if keyErr != nil {
		return nil, xcodes.ErrInternal.Wrap(keyErr)
	}

	// 2. checkQuota (read-only; no DB write)
	if checkErr := s.host.CheckQuota(ctx, s.db, ownerType, ownerID, file.size); checkErr != nil {
		return nil, xcodes.ErrQuotaExceeded.Wrap(checkErr)
	}

	// 3. session dedup + create
	session, err := s.findOrCreateSession(ctx, app, ownerType, ownerID, vendor, bucket, objectKey, file, ttl)
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
		AppKey:      app.AppKey,
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
	vendor      int32
	resolvedTTL time.Duration
}

// issueUploadCredential runs the STS credential flow:
//
//	1-4. shared prepareUpload prelude (dedup/quota/session/token)
//	5. STS credential (cached per owner+vendor+bucket)
func (s *Service) issueUploadCredential(ctx context.Context, app *models.StorageApp, ownerType int32, ownerID int64, bucket string, ttl time.Duration, file fileMeta) (*issueResult, error) {
	prepared, err := s.prepareUpload(ctx, app, ownerType, ownerID, bucket, ttl, file)
	if err != nil {
		return nil, err
	}
	if prepared.instant {
		return &issueResult{instant: true, fileID: prepared.fileID, fileInfo: prepared.fileInfo}, nil
	}

	stsPolicy := &storage.STSPolicy{
		OwnerID:   ownerID,
		OwnerType: ownerType,
		Bucket:    bucket,
		// Scope the credential to the owner's staging sandbox, not the bucket's
		// whole key prefix: a leaked credential can then only write into
		// (and at worst trash) that owner's own in-flight uploads. The temp key
		// issued above lives under this prefix, so the PUT still succeeds.
		//
		// AllowedExtensions is deliberately NOT forwarded: the client PUTs to a
		// server-chosen temp key with no file extension, so extension-shaped
		// resource patterns ("<prefix>/*.jpg") can never match it. Extension
		// policy is enforced at issue time by validateFilenameExtension instead.
		KeyPrefix:      conv.UploadSandboxPrefix(app.KeyPrefix, ownerType, ownerID),
		AllowedActions: []string{types.PutObjectActionForVendor(prepared.vendor)},
		MaxSize:        file.size,
		TTL:            prepared.resolvedTTL,
		// Hardening: HTTPS-only transport, force private object ACLs, and deny
		// post-upload ACL changes. Without DenyPutObjectACL a client could
		// promote its upload to public-read inside a private bucket.
		EnforceHTTPS:     true,
		LockObjectACL:    true,
		DenyPutObjectACL: true,
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
// Dedup is enforced solely by the Redis dedup lock — there is no DB unique
// constraint on these columns, keeping the schema portable across
// postgres/mysql/sqlite.
//
//  1. Redis dedup lock (s.lock, best-effort). Acquire outcomes:
//     - success: hold the lock for the rest of this call.
//     - redisx.ErrLockFailed (contention): fall through — the holder will
//     create the session, we pick it up via FindPendingDedup.
//     - any other error (Redis unreachable): fail closed.
//     When s.lock is nil (Redis not configured), dedup is skipped entirely —
//     an intentional best-effort deployment mode.
//
// Tradeoff: with no DB-level unique constraint, if Redis is unavailable and
// two callers race past FindPendingDedup, both may Create a PENDING session
// (duplicates expire via TTL). Accepted for DB portability — the former
// partial-unique-index backstop was postgres/sqlite only (mysql has none).
func (s *Service) findOrCreateSession(ctx context.Context, app *models.StorageApp, ownerType int32, ownerID int64, vendor int32, bucket, objectKey string, file fileMeta, ttl time.Duration) (*models.StorageUploadSession, error) {
	if s.lock != nil {
		target := dedupLockTarget(ownerType, ownerID, file.md5, file.size)
		lockID, lockErr := s.lock.Acquire(ctx, target)
		switch {
		case lockErr == nil:
			defer func() { _ = s.lock.Release(context.Background(), target, lockID) }()
		case errors.Is(lockErr, redisx.ErrLockFailed):
			// Another caller is creating the session; fall through to FindPendingDedup.
		default:
			// Fail closed: Redis configured but unreachable. Better to surface an
			// internal error than fall through and risk duplicate PENDING sessions.
			return nil, xcodes.ErrInternal.Wrapf(lockErr, "acquire upload dedup lock")
		}
	}
	// No lock held (Redis unavailable) or contention: fall through to
	// FindPendingDedup; if none exists we Create our own.

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

	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	session := &models.StorageUploadSession{
		ID:          id,
		OwnerType:   ownerType,
		OwnerID:     ownerID,
		Bucket:      bucket,
		ObjectKey:   objectKey,
		AppKey:      app.AppKey,
		KeyPrefix:   app.KeyPrefix,
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
	if err := dal.CreateUploadSession(ctx, s.db, session); err != nil {
		return nil, err
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
func (s *Service) handleInstantUpload(ctx context.Context, appKey string, ownerType int32, ownerID int64, existing *models.StorageObject, filename, filePath, description string, metadata map[string]string, isPublic bool, requestID string) (*storagev1.UserFileInfo, error) {
	if checkErr := s.host.CheckQuota(ctx, s.db, ownerType, ownerID, existing.Size); checkErr != nil {
		return nil, xcodes.ErrQuotaExceeded.Wrap(checkErr)
	}

	var fileInfo *storagev1.UserFileInfo
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		uf := &models.StorageFile{
			OwnerType:   ownerType,
			OwnerID:     ownerID,
			ObjectID:    existing.ID,
			AppKey:      appKey,
			Filename:    filename,
			FilePath:    filePath,
			Description: description,
			Metadata:    models.MapJSON(metadata),
			IsPublic:    isPublic,
		}
		if id, gidErr := gidservice.NextID(ctx, s.gid); gidErr != nil {
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

// dedupLockTarget builds the session-dedup lock TARGET — the per-domain suffix
// only. The configured LockConfig.Prefix is applied by the shared *redisx.Lock
// at Acquire time (redisx prefixes every key as "<Prefix>:<target>"), so these
// helpers intentionally do NOT include the prefix.
func dedupLockTarget(ownerType int32, ownerID int64, md5 string, size int64) string {
	return fmt.Sprintf("%s:%d:%d:%s:%d", lockTargetDedup, ownerType, ownerID, md5, size)
}

// objectLockTarget builds the object-dedup lock TARGET (suffix only; the prefix
// is applied by the lock instance — see dedupLockTarget).
func objectLockTarget(vendor int32, bucket, md5 string) string {
	return fmt.Sprintf("%s:%d:%s:%s", lockTargetObject, vendor, bucket, md5)
}
