package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/storage-service/internal/jobs"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/admin"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/service/file"
	"github.com/servekit/storage-service/internal/service/platform"
	"github.com/servekit/storage-service/internal/service/quota"
	"github.com/servekit/storage-service/internal/service/upload"
	"github.com/servekit/storage-service/internal/version"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/option"

	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/ratelimit"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	storagev1 "github.com/servekit/api/gen/go/storage/v1"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)

// StorageService holds business logic for the storage service. It no longer
// implements storagev1.StorageServiceServer directly — pkg/handler.Handler is
// the gRPC thin shell that delegates to these methods.
type StorageService struct {
	db       *gorm.DB
	redis    *redis.Client
	registry *storage.Registry
	gid      gidservice.Service
	limiter  ratelimit.Limiter
	cfg      *config.Config
	manager  *lifecycle.Manager

	audit  *audit.Service
	quota  *quota.Service
	file   *file.Service
	admin  *admin.Service
	upload *upload.Service

	// startedAt is set once in New; Ping returns it for uptime.
	startedAt int64
}

// Compile-time assertions that *StorageService satisfies its interface
// contracts: lifecycle.Service (Start/Stop driven by lifecycle.Manager) and
// upload.Host (so the upload subpackage can call back into the parent's
// quota/audit machinery without importing internal/service).
var (
	_ lifecycle.Service = (*StorageService)(nil)
	_ upload.Host       = (*StorageService)(nil)
)

// New creates a new StorageService from config with optional dependency injection.
func New(cfg *config.Config, opts ...option.Option) (*StorageService, error) {
	o := option.Apply(opts...)
	mgr := lifecycle.NewManager()

	db, err := dbx.Connect(cfg.Database, o.DB, mgr)
	if err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	gidGen, err := resolveGID(&o, thirdPartyGID(cfg), mgr)
	if err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	redisClient, err := resolveRedis(cfg, o.Redis, mgr)
	if err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	var limiter ratelimit.Limiter
	if redisClient != nil && rateLimitConfigured(cfg.Storage.RateLimit) {
		limiter = ratelimit.NewRedisLimiter(redisClient, cfg.Storage.RateLimit)
	}

	// Provider registry: DB-backed platform tables (storage_providers /
	// storage_buckets / storage_settings), rebuilt hot via the admin RPCs
	// and the cron convergence job. YAML providers are only read by the
	// one-shot `migrate --seed-from-config` importer.
	registry, err := storage.NewRegistry(nil)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init provider registry: %w", err), mgr.Stop())
	}
	if err := platform.LoadAndRebuild(context.Background(), db, registry, cfg.Storage.DefaultBucket, cfg.Storage.PublicBucket); err != nil {
		// Tables not migrated yet or a corrupt row: serve an empty registry
		// (uploads fail with clear bucket-not-found errors) instead of
		// refusing to boot; the cron job converges once fixed.
		slog.Error("initial platform registry load failed (empty until first refresh)", "error", err)
	}

	// Audit subpackage: owns Recorder/Event/snapshot types + the two read RPCs.
	// Its Recorder feeds the file/admin/upload subpackages via Deps injection.
	auditSvc := audit.New(&audit.Deps{DB: db, GID: gidGen})

	// Quota subpackage: owns the quota mechanism (check/reserve/release,
	// get/set/add, ensureQuota) plus the three quota RPCs. Built before
	// upload/file/admin so it can feed their quota calls via Deps injection.
	quotaSvc := quota.New(&quota.Deps{
		DB:                db,
		GID:               gidGen,
		Audit:             auditSvc.Recorder(),
		DefaultQuotaBytes: cfg.Storage.DefaultQuotaBytes,
	})

	// File subpackage: owns the 7 file-management RPCs (CRUD + download/process
	// URL generation). Depends on audit.Recorder and the quota Service via Deps
	// injection so it never imports the parent service package.
	fileSvc := file.New(&file.Deps{
		DB:            db,
		GID:           gidGen,
		Registry:      registry,
		Audit:         auditSvc.Recorder(),
		Quota:         quotaSvc,
		CDN:           cfg.Storage.CDN,
		PresignMaxTTL: cfg.Storage.PresignMaxTTL,
	})

	// Admin subpackage: owns the 10 admin RPCs (cross-owner file/quota/stats/
	// providers/buckets management + owner soft/hard delete) plus the cleanup
	// helpers (purge deleted objects/owners). Depends on audit.Recorder and the
	// quota Service via Deps injection so it never imports the parent package.
	adminSvc := admin.New(&admin.Deps{
		DB:       db,
		GID:      gidGen,
		Registry: registry,
		Audit:    auditSvc.Recorder(),
		Quota:    quotaSvc,
		Cfg:      cfg,
	})

	svc := &StorageService{
		db:        db,
		redis:     redisClient,
		registry:  registry,
		gid:       gidGen,
		limiter:   limiter,
		cfg:       cfg,
		audit:     auditSvc,
		quota:     quotaSvc,
		file:      fileSvc,
		admin:     adminSvc,
		manager:   mgr,
		startedAt: time.Now().UnixMilli(),
	}

	// Upload subpackage: holds its own sts (built from the registry inside
	// upload.New) and the dedup lock. svc is injected as the upload.Host so the
	// subpackage can call back into the parent's quota/audit machinery without
	// importing internal/service (no import cycle).
	svc.upload = upload.New(&upload.Deps{
		DB:       db,
		Registry: registry,
		GID:      gidGen,
		Cfg:      cfg,
		Limiter:  limiter,
		Redis:    redisClient,
		STS:      cfg.Storage.STS,
		Lock:     upload.NewLock(redisClient, cfg.Storage.UploadSession.Lock),
		Host:     svc,
	})

	// jobs.Scheduler owns the cron instance; setupJobs builds it, registers
	// it on mgr, and wires periodic jobs (reap, future sweeps, etc.).
	if err := svc.setupJobs(); err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	return svc, nil
}

// Registry returns the storage provider registry.
func (s *StorageService) Registry() *storage.Registry { return s.registry }

// Start starts lifecycle-managed components. For close-only resources (db,
// redis, gid) Start is a no-op.
func (s *StorageService) Start() error {
	return s.manager.Start()
}

// Stop stops all lifecycle-managed components. Stoppers (db/redis/gid) all
// run concurrently via lifecycle.Manager. Close errors are logged via
// slog.Warn inside each StopFunc; cleanup is best-effort.
func (s *StorageService) Stop() error {
	return s.manager.Stop()
}

// Ping is a health-check RPC. Returns only public, non-sensitive info.
func (s *StorageService) Ping(ctx context.Context) (*commonv1.Pong, error) {
	v := version.Get()
	return &commonv1.Pong{
		Service:   "storage-service",
		Version:   v.Version,
		GitCommit: v.GitCommit,
		GitBranch: v.GitBranch,
		BuildTime: v.BuildTime,
		GoVersion: v.GoVersion,
		Status:    "SERVING",
		Now:       time.Now().UnixMilli(),
		StartedAt: s.startedAt,
	}, nil
}

// --- upload.Host bridge ---
//
// *StorageService satisfies upload.Host so the upload subpackage can invoke the
// parent's quota and audit machinery without importing internal/service. These
// methods delegate to the existing unexported helpers.

// CheckQuota delegates to the quota subpackage for upload.Host.
func (s *StorageService) CheckQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID, requiredBytes int64) error {
	return s.quota.CheckQuota(ctx, db, ownerType, ownerID, requiredBytes)
}

// Reserve delegates to the quota subpackage for upload.Host.
func (s *StorageService) Reserve(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, bytes int64) error {
	return s.quota.Reserve(ctx, tx, ownerType, ownerID, bytes)
}

// RecordOutcome maps an upload.AuditEvent onto an audit.Event and records it.
// The Before/After maps pass through verbatim; only the subset of fields the
// upload domain populates is carried.
func (s *StorageService) RecordOutcome(ctx context.Context, event upload.AuditEvent, err error) {
	s.audit.Recorder().RecordOutcome(ctx, audit.Event{
		Action:     event.Action,
		OwnerType:  event.OwnerType,
		OwnerID:    event.OwnerID,
		TargetType: event.TargetType,
		TargetID:   event.TargetID,
		RequestID:  event.RequestID,
		Before:     event.Before,
		After:      event.After,
	}, err)
}

// --- upload domain facade ---
//
// Per golang-service-development skill §2, when a domain is upgraded to a
// subpackage the parent service.go exposes facade methods so pkg/handler never
// imports the subpackage directly. Each method is a one-line delegation.

// GenerateUploadURL delegates to the upload subpackage.
func (s *StorageService) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	return s.upload.GenerateUploadURL(ctx, req)
}

// ConfirmUpload delegates to the upload subpackage.
func (s *StorageService) ConfirmUpload(ctx context.Context, req *storagev1.ConfirmUploadRequest) (*storagev1.ConfirmUploadResponse, error) {
	return s.upload.ConfirmUpload(ctx, req)
}

// CancelUpload delegates to the upload subpackage.
func (s *StorageService) CancelUpload(ctx context.Context, req *storagev1.CancelUploadRequest) (*emptypb.Empty, error) {
	return s.upload.CancelUpload(ctx, req)
}

// GetSTSCredential delegates to the upload subpackage.
func (s *StorageService) GetSTSCredential(ctx context.Context, req *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	return s.upload.GetSTSCredential(ctx, req)
}

// BatchGetSTSCredential delegates to the upload subpackage.
func (s *StorageService) BatchGetSTSCredential(ctx context.Context, req *storagev1.BatchGetSTSCredentialRequest) (*storagev1.BatchGetSTSCredentialResponse, error) {
	return s.upload.BatchGetSTSCredential(ctx, req)
}

// --- audit domain facade ---

// ListMyAuditLogs delegates to the audit subpackage.
func (s *StorageService) ListMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	return s.audit.ListMyAuditLogs(ctx, req)
}

// AdminListAuditLogs delegates to the audit subpackage.
func (s *StorageService) AdminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	return s.audit.AdminListAuditLogs(ctx, req)
}

// --- quota domain facade ---

// GetMyQuota delegates to the quota subpackage.
func (s *StorageService) GetMyQuota(ctx context.Context, req *storagev1.GetMyQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.quota.GetMyQuota(ctx, req)
}

// SetOwnerQuota delegates to the quota subpackage.
func (s *StorageService) SetOwnerQuota(ctx context.Context, req *storagev1.SetOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.quota.SetOwnerQuota(ctx, req)
}

// AddOwnerQuota delegates to the quota subpackage.
func (s *StorageService) AddOwnerQuota(ctx context.Context, req *storagev1.AddOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.quota.AddOwnerQuota(ctx, req)
}

// --- file domain facade ---

// GenerateDownloadURL delegates to the file subpackage.
func (s *StorageService) GenerateDownloadURL(ctx context.Context, req *storagev1.GenerateDownloadURLRequest) (*storagev1.GenerateDownloadURLResponse, error) {
	return s.file.GenerateDownloadURL(ctx, req)
}

// CreateFileLink delegates to the file subpackage.
func (s *StorageService) CreateFileLink(ctx context.Context, req *storagev1.CreateFileLinkRequest) (*storagev1.CreateFileLinkResponse, error) {
	return s.file.CreateFileLink(ctx, req)
}

// GetFileLinkDownload delegates to the file subpackage.
func (s *StorageService) GetFileLinkDownload(ctx context.Context, req *storagev1.GetFileLinkDownloadRequest) (*storagev1.GetFileLinkDownloadResponse, error) {
	return s.file.GetFileLinkDownload(ctx, req)
}

// ListMyFiles delegates to the file subpackage.
func (s *StorageService) ListMyFiles(ctx context.Context, req *storagev1.ListMyFilesRequest) (*storagev1.ListMyFilesResponse, error) {
	return s.file.ListMyFiles(ctx, req)
}

// ListMyFilesPaged delegates to the file subpackage.
func (s *StorageService) ListMyFilesPaged(ctx context.Context, req *storagev1.ListMyFilesPagedRequest) (*storagev1.ListMyFilesPagedResponse, error) {
	return s.file.ListMyFilesPaged(ctx, req)
}

// GetMyFile delegates to the file subpackage.
func (s *StorageService) GetMyFile(ctx context.Context, req *storagev1.GetMyFileRequest) (*storagev1.UserFileInfo, error) {
	return s.file.GetMyFile(ctx, req)
}

// UpdateMyFile delegates to the file subpackage.
func (s *StorageService) UpdateMyFile(ctx context.Context, req *storagev1.UpdateMyFileRequest) (*storagev1.UserFileInfo, error) {
	return s.file.UpdateMyFile(ctx, req)
}

// DeleteMyFile delegates to the file subpackage.
func (s *StorageService) DeleteMyFile(ctx context.Context, req *storagev1.DeleteMyFileRequest) (*emptypb.Empty, error) {
	return s.file.DeleteMyFile(ctx, req)
}

// BatchDeleteMyFiles delegates to the file subpackage.
func (s *StorageService) BatchDeleteMyFiles(ctx context.Context, req *storagev1.BatchDeleteMyFilesRequest) (*storagev1.BatchDeleteMyFilesResponse, error) {
	return s.file.BatchDeleteMyFiles(ctx, req)
}

// GenerateProcessURL delegates to the file subpackage.
func (s *StorageService) GenerateProcessURL(ctx context.Context, req *storagev1.GenerateProcessURLRequest) (*storagev1.GenerateProcessURLResponse, error) {
	return s.file.GenerateProcessURL(ctx, req)
}

// GenerateCDNURL delegates to the file subpackage.
func (s *StorageService) GenerateCDNURL(ctx context.Context, req *storagev1.GenerateCDNURLRequest) (*storagev1.GenerateCDNURLResponse, error) {
	return s.file.GetCDNURL(ctx, req)
}

// --- admin domain facade ---

// AdminListFiles delegates to the admin subpackage.
func (s *StorageService) AdminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	return s.admin.AdminListFiles(ctx, req)
}

// AdminGetFile delegates to the admin subpackage.
func (s *StorageService) AdminGetFile(ctx context.Context, req *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	return s.admin.AdminGetFile(ctx, req)
}

// AdminDeleteFile delegates to the admin subpackage.
func (s *StorageService) AdminDeleteFile(ctx context.Context, req *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	return s.admin.AdminDeleteFile(ctx, req)
}

// AdminGetQuota delegates to the admin subpackage.
func (s *StorageService) AdminGetQuota(ctx context.Context, req *storagev1.AdminGetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.admin.AdminGetQuota(ctx, req)
}

// AdminSetQuota delegates to the admin subpackage.
func (s *StorageService) AdminSetQuota(ctx context.Context, req *storagev1.AdminSetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.admin.AdminSetQuota(ctx, req)
}

// AdminGetStats delegates to the admin subpackage.
func (s *StorageService) AdminGetStats(ctx context.Context, req *storagev1.AdminGetStatsRequest) (*storagev1.AdminGetStatsResponse, error) {
	return s.admin.AdminGetStats(ctx, req)
}

// AdminListProviders delegates to the admin subpackage.
func (s *StorageService) AdminListProviders(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	return s.admin.AdminListProviders(ctx, req)
}

// AdminListBuckets delegates to the admin subpackage.
func (s *StorageService) AdminListBuckets(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	return s.admin.AdminListBuckets(ctx, req)
}

// AdminCreateProvider delegates to the admin subpackage (live registry
// rebuilds immediately on success).
func (s *StorageService) AdminCreateProvider(ctx context.Context, req *storagev1.AdminCreateProviderRequest) (*storagev1.AdminCreateProviderResponse, error) {
	return s.admin.AdminCreateProvider(ctx, req)
}

// AdminUpdateProvider delegates to the admin subpackage.
func (s *StorageService) AdminUpdateProvider(ctx context.Context, req *storagev1.AdminUpdateProviderRequest) (*storagev1.AdminUpdateProviderResponse, error) {
	return s.admin.AdminUpdateProvider(ctx, req)
}

// AdminDeleteProvider delegates to the admin subpackage (rejected while
// buckets are still bound).
func (s *StorageService) AdminDeleteProvider(ctx context.Context, req *storagev1.AdminDeleteProviderRequest) (*emptypb.Empty, error) {
	return s.admin.AdminDeleteProvider(ctx, req)
}

// AdminUpsertBucket delegates to the admin subpackage (full replace;
// rebind guarded while objects exist).
func (s *StorageService) AdminUpsertBucket(ctx context.Context, req *storagev1.AdminUpsertBucketRequest) (*storagev1.AdminUpsertBucketResponse, error) {
	return s.admin.AdminUpsertBucket(ctx, req)
}

// AdminCreateApp delegates to the admin subpackage.
func (s *StorageService) AdminCreateApp(ctx context.Context, req *storagev1.AdminCreateAppRequest) (*storagev1.AdminCreateAppResponse, error) {
	return s.admin.AdminCreateApp(ctx, req)
}

// AdminGetApp delegates to the admin subpackage.
func (s *StorageService) AdminGetApp(ctx context.Context, req *storagev1.AdminGetAppRequest) (*storagev1.AdminGetAppResponse, error) {
	return s.admin.AdminGetApp(ctx, req)
}

// AdminUpdateApp delegates to the admin subpackage.
func (s *StorageService) AdminUpdateApp(ctx context.Context, req *storagev1.AdminUpdateAppRequest) (*storagev1.AdminUpdateAppResponse, error) {
	return s.admin.AdminUpdateApp(ctx, req)
}

// AdminRotateAppSecret delegates to the admin subpackage.
func (s *StorageService) AdminRotateAppSecret(ctx context.Context, req *storagev1.AdminRotateAppSecretRequest) (*storagev1.AdminRotateAppSecretResponse, error) {
	return s.admin.AdminRotateAppSecret(ctx, req)
}

// AdminListApps delegates to the admin subpackage.
func (s *StorageService) AdminListApps(ctx context.Context, req *storagev1.AdminListAppsRequest) (*storagev1.AdminListAppsResponse, error) {
	return s.admin.AdminListApps(ctx, req)
}

// AdminDeleteApp delegates to the admin subpackage.
func (s *StorageService) AdminDeleteApp(ctx context.Context, req *storagev1.AdminDeleteAppRequest) (*emptypb.Empty, error) {
	return s.admin.AdminDeleteApp(ctx, req)
}

// AdminDeleteBucket delegates to the admin subpackage (rejected while
// objects exist).
func (s *StorageService) AdminDeleteBucket(ctx context.Context, req *storagev1.AdminDeleteBucketRequest) (*emptypb.Empty, error) {
	return s.admin.AdminDeleteBucket(ctx, req)
}

// AdminGetSettings delegates to the admin subpackage.
func (s *StorageService) AdminGetSettings(ctx context.Context, req *storagev1.AdminGetSettingsRequest) (*storagev1.AdminGetSettingsResponse, error) {
	return s.admin.AdminGetSettings(ctx, req)
}

// AdminUpdateSettings delegates to the admin subpackage.
func (s *StorageService) AdminUpdateSettings(ctx context.Context, req *storagev1.AdminUpdateSettingsRequest) (*storagev1.AdminUpdateSettingsResponse, error) {
	return s.admin.AdminUpdateSettings(ctx, req)
}

// AdminSoftDeleteOwnerFiles delegates to the admin subpackage.
func (s *StorageService) AdminSoftDeleteOwnerFiles(ctx context.Context, req *storagev1.AdminSoftDeleteOwnerFilesRequest) (*storagev1.AdminSoftDeleteOwnerFilesResponse, error) {
	return s.admin.AdminSoftDeleteOwnerFiles(ctx, req)
}

// AdminDeleteOwner delegates to the admin subpackage.
func (s *StorageService) AdminDeleteOwner(ctx context.Context, req *storagev1.AdminDeleteOwnerRequest) (*storagev1.AdminDeleteOwnerResponse, error) {
	return s.admin.AdminDeleteOwner(ctx, req)
}

// setupJobs builds the jobs.Scheduler, registers it on s.manager (so its
// lifecycle is managed alongside db/redis/gid), and wires periodic jobs.
// Signature is intentionally receiver-only: future jobs are added inside
// this method as additional scheduler.AddFunc calls — callers never need
// to extend a parameter list.
func (s *StorageService) setupJobs() error {
	scheduler, err := jobs.New(&jobs.Deps{
		Config: &cronx.Config{
			Timezone:      s.cfg.Storage.Cron.Timezone,
			OverlapPolicy: "skip",
		},
	})
	if err != nil {
		return fmt.Errorf("init jobs: %w", err)
	}
	s.manager.Add("jobs", scheduler)

	// Periodic reap of expired upload sessions: scans PENDING rows past
	// expiry, reclaims OSS orphans. CronSpec default lives in
	// config.UploadGCConfig's default tag.
	if err := scheduler.AddFunc(s.cfg.Storage.UploadGC.CronSpec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := s.upload.ReapExpiredSessions(ctx); err != nil {
			slog.Error("upload reap", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("register upload reap: %w", err)
	}

	// Two-stage retention GC for linkd files: mark expired (quota +
	// ref-count release), then purge tombstones after the recovery window.
	if err := scheduler.AddFunc(s.cfg.Storage.FileRetentionGC.CronSpec, func() {
		cfg := s.cfg.Storage.FileRetentionGC
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := s.file.ReapExpiredFiles(ctx, cfg.HardDeleteAfter, cfg.BatchSize); err != nil {
			slog.Error("file retention reap", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("register file retention reap: %w", err)
	}

	// Platform registry convergence: cross-node admin mutations (or a
	// failed local refresh) rebuild within a minute.
	if err := scheduler.AddFunc("*/1 * * * *", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := platform.LoadAndRebuild(ctx, s.db, s.registry, "", ""); err != nil {
			slog.Error("platform registry cron refresh", "error", err)
		}
	}); err != nil {
		return fmt.Errorf("register platform refresh: %w", err)
	}
	return nil
}
