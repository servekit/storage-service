package service

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/redisx"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/fake"
	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/service/file"
	"github.com/servekit/storage-service/internal/service/quota"
	"github.com/servekit/storage-service/internal/service/upload"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/internal/thirdcall/gid_service"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const testSecret = "test-secret-key-12345"

// --- buildAdminFileInfo / AdminListProviders / AdminListBuckets tests ---
//
// These unit tests moved to the admin subpackage
// (internal/service/admin/admin_test.go) where the admin domain lives after
// the Phase 4 extraction.

// --- checkUploadRateLimit tests ---
//
// The no-limiter fast path and rate-limit enforcement are now unit-tested in
// the upload subpackage (internal/service/upload/token_test.go), where
// checkUploadRateLimit lives after the Phase 3 split.

// --- lifecycle tests ---

// TestStart_idempotent verifies Start returns nil on a freshly constructed
// service and is idempotent across multiple calls (lifecycle.Manager uses
// sync.Once).
func TestStart_idempotent(t *testing.T) {
	svc := &StorageService{}
	setupManagerForTest(svc)

	assert.NoError(t, svc.Start())
	// Second call must not panic or error.
	assert.NoError(t, svc.Start())
}

// TestStop_releasesOwnedDB verifies Stop closes an owned *sql.DB and returns nil.
func TestStop_releasesOwnedDB(t *testing.T) {
	db, sqlDB, err := openOwnedTestDB(t)
	require.NoError(t, err)

	svc := &StorageService{db: db}
	setupManagerForTest(svc)
	// Simulate the "self-built DB" path: register a Stopper that closes the
	// pool, mirroring resolveDB's behavior when WithDB is not supplied.
	svc.manager.AddStopper("db", lifecycle.StopFunc(func() {
		sqlDB.Close()
	}))

	err = svc.Stop()
	assert.NoError(t, err)
	// After Stop the underlying *sql.DB must reject further operations. The
	// exact error string is driver-specific (stdlib returns sql.ErrConnDone
	// for most drivers, but sqlite3 reports "database is closed"), so we only
	// assert that Ping fails.
	assert.Error(t, sqlDB.Ping(), "underlying sql.DB must be closed")
}

// TestStop_skipsNilResources verifies Stop returns nil when the manager has
// no registered Stoppers (the new-model equivalent of "ownDB/ownRedis true but
// nil resources are skipped").
func TestStop_skipsNilResources(t *testing.T) {
	svc := &StorageService{}
	setupManagerForTest(svc)

	err := svc.Stop()
	assert.NoError(t, err, "empty manager returns nil from Stop")
}

// TODO: Integration tests (require PostgreSQL/Redis/minio):
// - adminListFiles, adminGetFile, adminDeleteFile, adminGetQuota, adminSetQuota,
//   adminGetStats, adminSoftDeleteOwnerFiles, adminDeleteOwner (PostgreSQL via dbx.SetupTestDB)
// - checkUploadRateLimit enforcement (Redis via miniredis)
// - generateUploadURL / confirmUpload end-to-end (MinIO testcontainer)
// See CLAUDE.md for integration test patterns.

// --- internal helpers ---

// newTestRegistry builds a storage.Registry with two s3_compatible providers
// (no cloud credentials needed — registry only stores config).
func newTestRegistry(t *testing.T) *storage.Registry {
	t.Helper()

	cfg := []*config.ProviderConfig{
		{
			Name:      "minio-local",
			Vendor:    "VENDOR_S3_COMPATIBLE",
			Endpoint:  "http://localhost:9000",
			Region:    "us-east-1",
			AccessKey: "test-access",
			SecretKey: "test-secret",
			Buckets: []*config.BucketConfig{
				{Name: "uploads", KeyPrefix: "uploads/", ACL: "private"},
				{Name: "assets", KeyPrefix: "assets/", ACL: "public_read"},
			},
		},
		{
			Name:      "wasabi-backup",
			Vendor:    "VENDOR_S3_COMPATIBLE",
			Endpoint:  "https://s3.wasabisys.com",
			Region:    "us-east-1",
			AccessKey: "test-access-2",
			SecretKey: "test-secret-2",
			Buckets: []*config.BucketConfig{
				{Name: "backups", KeyPrefix: "backup/", ACL: "public_read_write"},
			},
		},
	}

	registry, err := storage.NewRegistry(cfg)
	require.NoError(t, err, "NewRegistry should succeed with s3_compatible provider")
	return registry
}

// setupManagerForTest initializes the lifecycle.Manager field for a directly
// constructed StorageService. Tests that bypass New() (which has external
// dependencies) call this to get a usable Start/Stop.
func setupManagerForTest(svc *StorageService) {
	svc.manager = lifecycle.NewManager()
}

// openOwnedTestDB returns an in-memory sqlite *gorm.DB plus its underlying
// *sql.DB so the test can assert the sql.DB is closed after Stop.
//
// We use sqlite because it has no external process; storage-service normally
// uses postgres via dbx, but this test only verifies Stop's close semantics
// and does not need postgres features.
func openOwnedTestDB(t *testing.T) (*gorm.DB, *sql.DB, error) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		return nil, nil, err
	}
	underlying, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	return db, underlying, nil
}

// --- GetSTSCredential integration tests ---

// seqGID is a gid_service.GIDService implementation returning sequential IDs
// without any external dependency. Safe for concurrent use.
type seqGID struct {
	counter int64
}

func (g *seqGID) NextID(_ context.Context) (int64, error) {
	return atomic.AddInt64(&g.counter, 1), nil
}

func (g *seqGID) Close() error { return nil }

// fakeSTSIssuerRecorder returns a fixed STSCredential and records the policy
// passed to it so tests can assert on TTL. Mirrors the type used in
// sts_cache_test.go but local to this file to avoid coupling.
type fakeSTSIssuerRecorder struct {
	mu     sync.Mutex
	creds  *storage.STSCredential
	policy *storage.STSPolicy
	calls  int
}

func (f *fakeSTSIssuerRecorder) Issue(_ context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.policy = policy
	return f.creds, nil
}

func (f *fakeSTSIssuerRecorder) lastPolicy() *storage.STSPolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy
}

// setupServiceWithFakeProvider builds a StorageService wired to a real
// Postgres testcontainer, real miniredis, and a fake STS issuer. The testcontainer
// and miniredis both register their teardown via t.Cleanup internally, so no
// explicit cleanup func is returned.
//
// The upload subpackage is constructed via upload.New and wired as svc.upload so
// the facade methods (svc.GetSTSCredential, etc.) hit the real upload.Service.
// svc is injected as upload.Host so quota/audit calls route back into the parent.
func setupServiceWithFakeProvider(t *testing.T) *StorageService {
	t.Helper()

	db := dbx.SetupTestDB(t)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	rdb := redisx.NewTestClient(t)

	registry := newTestRegistry(t)
	gid := &seqGID{}

	cfg := &config.Config{
		Storage: &config.StorageConfig{
			UploadTokenTTL:    30 * time.Minute,
			UploadTokenSecret: testSecret,
			DefaultQuotaBytes: 1 << 30, // 1GB; plenty for tests
			DefaultBucket:     "uploads",
			Providers:         nil,
			Batch:             &config.BatchConfig{},
			UploadGC:          &config.UploadGCConfig{BatchSize: 100},
		},
	}

	svc := &StorageService{
		db:       db,
		redis:    rdb,
		registry: registry,
		gid:      gid,
		cfg:      cfg,
		audit:    audit.New(&audit.Deps{DB: db, GID: gid}),
		quota:    quota.New(&quota.Deps{DB: db, GID: gid, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder(), DefaultQuotaBytes: 1 << 30}),
	}
	svc.upload = upload.New(&upload.Deps{
		DB:        db,
		Registry:  registry,
		GID:       gid,
		Cfg:       cfg,
		Redis:     rdb,
		STS:       &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour},
		DedupLock: upload.NewDedupLock(rdb, &config.LockConfig{}),
		Host:      svc,
	})
	// Wire a fake STS issuer so GetSTSCredential / BatchGetSTSCredential work
	// without a real S3-compatible endpoint (S3Provider.GetSTSToken intentionally
	// returns "STS not supported"). Tests that need to assert on the policy or
	// override the credential call upload.SetSTS themselves, replacing this.
	upload.SetSTS(svc.upload, rdb, &fakeSTSIssuerRecorder{
		creds: &storage.STSCredential{
			AccessKey:       "ak-fake",
			SecretKey:       "sk-fake",
			SecurityToken:   "st-fake",
			Endpoint:        "http://fake-endpoint",
			Bucket:          "uploads",
			ObjectKeyPrefix: "uploads/",
			ExpiresAt:       time.Now().Add(30 * time.Minute),
		},
	}, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	return svc
}

func TestGetSTSCredential_CreatesSession(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)

	resp, err := svc.GetSTSCredential(context.Background(), &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     1024,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.False(t, resp.GetInstant())
	assert.NotEmpty(t, resp.GetUploadToken())

	tok, err := upload.VerifyTokenForTest(resp.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	require.NotZero(t, tok.SessionID)

	session, err := dal.GetUploadSessionByID(context.Background(), svc.db, tok.SessionID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING)), session.Status)
	assert.Equal(t, "00000000000000000000000000000001", session.MD5)
	assert.Equal(t, int64(1024), session.Size)

	// Verify the SESSION_CREATE audit event was recorded with the expected After snapshot.
	logs, _, err := dal.ListAuditLogsByOwner(context.Background(), svc.db, 1, 100, dal.AuditLogFilter{
		Action:     int32(storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_CREATE),
		Pagination: dbx.Pagination{PageSize: 10},
	})
	require.NoError(t, err)
	require.Len(t, logs, 1, "exactly one SESSION_CREATE audit entry expected")
	assert.Equal(t, int32(storagev1.AuditAction_AUDIT_ACTION_UPLOAD_SESSION_CREATE), logs[0].Action)
	assert.Equal(t, session.ID, logs[0].TargetID)
	require.NotNil(t, logs[0].After)
	afterMD5, _ := logs[0].After["md5"].(string)
	assert.Equal(t, "00000000000000000000000000000001", afterMD5)
	// JSON numbers unmarshal to float64.
	afterVendor, _ := logs[0].After["vendor"].(float64)
	assert.Equal(t, float64(session.Vendor), afterVendor)
}

func TestGetSTSCredential_DedupReuseSession(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)

	req := &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 200},
		Bucket:      "uploads",
		MaxSize:     2048,
		Md5:         "00000000000000000000000000000002",
		ContentType: "text/plain",
		Filename:    "b.txt",
	}

	resp1, err := svc.GetSTSCredential(context.Background(), req)
	require.NoError(t, err)

	resp2, err := svc.GetSTSCredential(context.Background(), req)
	require.NoError(t, err)

	tok1, err := upload.VerifyTokenForTest(resp1.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 200, 1)
	require.NoError(t, err)
	tok2, err := upload.VerifyTokenForTest(resp2.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 200, 1)
	require.NoError(t, err)

	assert.Equal(t, tok1.SessionID, tok2.SessionID, "duplicate request should reuse the same session")
}

func TestGetSTSCredential_TTLFromRequest(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)

	// Use a custom issuer so we can read the TTL it observed.
	issuer := &fakeSTSIssuerRecorder{
		creds: &storage.STSCredential{
			AccessKey:       "ak",
			SecretKey:       "sk",
			SecurityToken:   "st",
			Endpoint:        "http://e",
			Bucket:          "uploads",
			ObjectKeyPrefix: "uploads/",
			ExpiresAt:       time.Now().Add(time.Hour),
		},
	}
	upload.SetSTS(svc.upload, svc.redis, issuer, &config.STSConfig{
		DefaultTTL: 15 * time.Minute,
		MaxTTL:     time.Hour,
	})

	resp, err := svc.GetSTSCredential(context.Background(), &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 300},
		Bucket:      "uploads",
		MaxSize:     512,
		Md5:         "00000000000000000000000000000003",
		ContentType: "text/plain",
		Filename:    "c.txt",
		Ttl:         durationpb.New(30 * time.Second),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetUploadToken())

	policy := issuer.lastPolicy()
	require.NotNil(t, policy)
	assert.Equal(t, 30*time.Second, policy.TTL)
}

// Compile-time assertion that *seqGID satisfies gid_service.GIDService.
var _ gid_service.GIDService = (*seqGID)(nil)

func TestBatchGetSTSCredential_AllSucceed(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)

	resp, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket: "uploads",
		Ttl:    durationpb.New(15 * time.Minute),
		Files: []*storagev1.UploadFileMeta{
			{Md5: "00000000000000000000000000000001", Size: 100, Filename: "a.txt", ContentType: "text/plain"},
			{Md5: "00000000000000000000000000000002", Size: 100, Filename: "b.txt", ContentType: "text/plain"},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.GetItems(), 2)
	// No quota configured = unlimited; both items should succeed.
	for i, item := range resp.GetItems() {
		switch r := item.GetResult().(type) {
		case *storagev1.UploadCredentialItem_Token:
			assert.NotEmpty(t, r.Token.GetUploadToken(), "item %d should have token", i)
		case *storagev1.UploadCredentialItem_Error:
			t.Errorf("item %d unexpectedly failed: %s", i, r.Error.GetMessage())
		}
	}
	assert.NotEmpty(t, resp.GetAccessKey(), "shared STS access_key should be set")
}

// TestBatchGetSTSCredential_PartialFailure exercises the per-item error path:
// one file passes quota check while another exceeds it. checkQuota is read-only
// (reserve happens later at ConfirmUpload), so both items independently evaluate
// against the same starting available bytes. With a 150-byte quota, a 100-byte
// file passes and a 200-byte file fails with QUOTA_EXCEEDED — verifying that the
// RPC returns OK with per-item ItemError entries instead of failing the batch.
func TestBatchGetSTSCredential_PartialFailure(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)
	ctx := context.Background()

	const (
		ownerType = storagev1.OwnerType_OWNER_TYPE_USER
		ownerID   = int64(500)
	)
	// Tight quota: 150 bytes total, 0 used -> 150 available.
	require.NoError(t, svc.quota.SetQuota(ctx, svc.db, int32(ownerType), ownerID, 150))

	resp, err := svc.BatchGetSTSCredential(ctx, &storagev1.BatchGetSTSCredentialRequest{
		Owner:  &storagev1.Owner{OwnerType: ownerType, OwnerId: ownerID},
		Bucket: "uploads",
		Ttl:    durationpb.New(15 * time.Minute),
		Files: []*storagev1.UploadFileMeta{
			{Md5: "00000000000000000000000000000001", Size: 100, Filename: "fits.txt", ContentType: "text/plain"},
			{Md5: "00000000000000000000000000000002", Size: 200, Filename: "too-big.txt", ContentType: "text/plain"},
		},
	})
	require.NoError(t, err, "RPC must not error; per-item failure goes in items")
	require.Len(t, resp.GetItems(), 2)

	// Item 0: 100 bytes fits in 150-byte quota -> success.
	tokenItem, ok := resp.GetItems()[0].GetResult().(*storagev1.UploadCredentialItem_Token)
	require.True(t, ok, "item 0 should succeed; got %T", resp.GetItems()[0].GetResult())
	assert.NotEmpty(t, tokenItem.Token.GetUploadToken(), "item 0 should have a token")

	// Item 1: 200 bytes exceeds 150-byte quota -> per-item error.
	errItem, ok := resp.GetItems()[1].GetResult().(*storagev1.UploadCredentialItem_Error)
	require.True(t, ok, "item 1 should fail with quota exceeded; got %T", resp.GetItems()[1].GetResult())
	assert.Equal(t, "QUOTA_EXCEEDED", errItem.Error.GetCode(), "ItemError.Code should be the xerr Reason")
	assert.NotEmpty(t, errItem.Error.GetMessage(), "ItemError.Message should be populated")
}

func TestBatchGetSTSCredential_SharedSTSCredential(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)

	// Custom issuer so we can count issuer calls.
	issuer := &fakeSTSIssuerRecorder{
		creds: &storage.STSCredential{
			AccessKey:       "ak-batch",
			SecretKey:       "sk-batch",
			SecurityToken:   "st-batch",
			Endpoint:        "http://batch",
			Bucket:          "uploads",
			ObjectKeyPrefix: "uploads/",
			ExpiresAt:       time.Now().Add(30 * time.Minute),
		},
	}
	upload.SetSTS(svc.upload, svc.redis, issuer, &config.STSConfig{
		DefaultTTL: 15 * time.Minute,
		MaxTTL:     time.Hour,
	})

	const n = 5
	files := make([]*storagev1.UploadFileMeta, 0, n)
	for i := 0; i < n; i++ {
		files = append(files, &storagev1.UploadFileMeta{
			Md5:         fmt.Sprintf("%032d", i+1),
			Size:        100,
			Filename:    fmt.Sprintf("file-%d.txt", i),
			ContentType: "text/plain",
		})
	}

	resp, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 200},
		Bucket: "uploads",
		Ttl:    durationpb.New(15 * time.Minute),
		Files:  files,
	})
	require.NoError(t, err)
	require.Len(t, resp.GetItems(), n)
	// Only 1 issuer call regardless of file count: per-file calls hit the cache.
	assert.Equal(t, 1, issuer.calls, "expected exactly 1 STS issuer call for the whole batch, got %d", issuer.calls)
}

func TestBatchGetSTSCredential_TooManyFiles(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)
	svc.cfg.Storage.Batch.MaxSize = 2

	files := []*storagev1.UploadFileMeta{
		{Md5: "00000000000000000000000000000001", Size: 100, Filename: "a.txt", ContentType: "text/plain"},
		{Md5: "00000000000000000000000000000002", Size: 100, Filename: "b.txt", ContentType: "text/plain"},
		{Md5: "00000000000000000000000000000003", Size: 100, Filename: "c.txt", ContentType: "text/plain"},
	}

	_, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 300},
		Bucket: "uploads",
		Files:  files,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FILE_BATCH_TOO_LARGE")
}

func TestBatchGetSTSCredential_EmptyFiles(t *testing.T) {
	svc := setupServiceWithFakeProvider(t)

	_, err := svc.BatchGetSTSCredential(context.Background(), &storagev1.BatchGetSTSCredentialRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 400},
		Bucket: "uploads",
		Files:  nil,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}

// --- ConfirmUpload integration tests ---
//
// These tests need a real Provider implementation that can answer HeadObject,
// so they wire a fake.FakeProvider into the registry via
// setupServiceWithFakeObjectProvider. GetSTSCredential / ConfirmUpload flow
// end-to-end through the same code paths the real S3 provider would exercise.

// setupServiceWithFakeObjectProvider is like setupServiceWithFakeProvider but
// swaps the registry for one backed by an in-memory FakeProvider so HeadObject
// works without a real object store. The FakeProvider is returned for direct
// mutation in tests (e.g. simulating an OSS upload).
func setupServiceWithFakeObjectProvider(t *testing.T) (*StorageService, *fake.FakeProvider) {
	t.Helper()

	db := dbx.SetupTestDB(t)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	rdb := redisx.NewTestClient(t)

	fp := fake.NewFakeProvider()
	fp.SetSTSCredential(&storage.STSCredential{
		AccessKey:       "ak-fake",
		SecretKey:       "sk-fake",
		SecurityToken:   "st-fake",
		Endpoint:        "http://fake-endpoint",
		Bucket:          "uploads",
		ObjectKeyPrefix: "uploads/",
		ExpiresAt:       time.Now().Add(30 * time.Minute),
	})

	providerCfg := &config.ProviderConfig{
		Name:      "fake-local",
		Vendor:    "VENDOR_S3_COMPATIBLE",
		Endpoint:  "http://fake-endpoint",
		Region:    "us-east-1",
		AccessKey: "ak-fake",
		SecretKey: "sk-fake",
		Buckets: []*config.BucketConfig{
			{Name: "uploads", KeyPrefix: "uploads/", ACL: "private"},
		},
	}
	registry, err := storage.NewRegistryWithProvider(providerCfg, fp, map[string]types.CDNURLGenerator{
		"uploads": fake.NewCDNURLGenerator(),
	})
	require.NoError(t, err)

	gid := &seqGID{}

	cfg := &config.Config{
		Storage: &config.StorageConfig{
			UploadTokenTTL:    30 * time.Minute,
			UploadTokenSecret: testSecret,
			DefaultQuotaBytes: 1 << 30,
			DefaultBucket:     "uploads",
			UploadGC:          &config.UploadGCConfig{BatchSize: 100},
			CDN: config.CDNRuntimeConfig{
				DefaultTTL: time.Hour,
				MinTTL:     5 * time.Minute,
				MaxTTL:     24 * time.Hour,
			},
		},
	}

	svc := &StorageService{
		db:       db,
		redis:    rdb,
		registry: registry,
		gid:      gid,
		cfg:      cfg,
		audit:    audit.New(&audit.Deps{DB: db, GID: gid}),
		quota:    quota.New(&quota.Deps{DB: db, GID: gid, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder(), DefaultQuotaBytes: 1 << 30}),
		file:     file.New(&file.Deps{DB: db, GID: gid, Registry: registry, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder(), Quota: quota.New(&quota.Deps{DB: db, GID: gid, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder(), DefaultQuotaBytes: 1 << 30}), CDN: cfg.Storage.CDN}),
	}
	svc.upload = upload.New(&upload.Deps{
		DB:        db,
		Registry:  registry,
		GID:       gid,
		Cfg:       cfg,
		Redis:     rdb,
		STS:       &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour},
		DedupLock: upload.NewDedupLock(rdb, &config.LockConfig{}),
		Host:      svc,
	})
	return svc, fp
}

// fakeUploadToProvider simulates the client-side OSS PUT by writing the bytes
// into the FakeProvider with the declared MD5 pinned as the ETag and the
// session's ContentType recorded on the fake object. contentType MUST match
// what was declared in the upload_token/session — ConfirmUpload now rejects
// mismatches, so callers typically pass session.ContentType.
func fakeUploadToProvider(fp *fake.FakeProvider, bucket, key string, data []byte, md5, contentType string) {
	fp.PutObjectWithMD5(context.Background(), bucket, key, data, contentType, md5)
}

func TestConfirmUpload_Idempotent(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	// 1. Issue token (creates session).
	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	// Resolve the real object key from the session (the response's ObjectKey
	// field only carries the prefix for legacy compatibility).
	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)

	// 2. Simulate OSS upload (fake provider remembers the bytes).
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"), "00000000000000000000000000000001", session.ContentType)

	// 3. First confirm.
	r1, err := svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)
	fileID := r1.GetFileId()
	require.NotZero(t, fileID)

	// 4. Second confirm — same token, must return same FileID without creating a new file.
	r2, err := svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)
	assert.Equal(t, fileID, r2.GetFileId())

	// 5. Verify only one file row exists for this owner.
	files, err := dal.CountFilesByOwner(ctx, svc.db, 100, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), files)
}

func TestConfirmUpload_LegacyTokenRejected(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	// Build a token with SessionID=0 (legacy).
	tok := upload.TokenForTest()
	tok.SessionID = 0
	tok.OwnerID = 100
	tok.OwnerType = 1
	tok.MD5 = "00000000000000000000000000000002"
	tok.Size = 4
	tok.ContentType = "text/plain"
	tok.Bucket = "uploads"
	tok.Vendor = int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE)
	tok.Filename = "legacy.txt"
	tok.ExpiresAt = time.Now().Add(30 * time.Minute).Unix()
	tokStr, err := upload.SignTokenForTest(tok, svc.cfg.Storage.UploadTokenSecret)
	require.NoError(t, err)

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: tokStr,
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UPLOAD_TOKEN_INVALID")
}

func TestConfirmUpload_SessionTokenMismatch(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	// Tamper: forge a token pointing at the real session but with a different MD5.
	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)

	// Provide OSS bytes for the original md5 — irrelevant since the cross-check fires first.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"), "00000000000000000000000000000001", session.ContentType)

	tampered := *verified
	tampered.MD5 = "ffffffffffffffffffffffffffffffff"
	tamperedStr, err := upload.SignTokenForTest(&tampered, svc.cfg.Storage.UploadTokenSecret)
	require.NoError(t, err)

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: tamperedStr,
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UPLOAD_TOKEN_INVALID")
}

func TestConfirmUpload_ExpiredSession(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"), "00000000000000000000000000000001", session.ContentType)

	// Force the session into EXPIRED status.
	claimed, err := dal.MarkUploadSessionExpired(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)
	require.True(t, claimed, "MarkExpired must claim a PENDING session on first CAS")

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UPLOAD_SESSION_EXPIRED")
}

// TestConfirmUpload_ContentTypeMismatch verifies ConfirmUpload rejects an
// upload whose cloud-side Content-Type disagrees with what was declared in
// the upload_token/session. Without this check, the StorageObject would be
// persisted with the declared type even though the bytes are something else,
// causing later serve calls to mis-label the file.
func TestConfirmUpload_ContentTypeMismatch(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)

	// Client uploads bytes with a Content-Type the cloud records as image/jpeg,
	// contradicting the session's text/plain.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"),
		"00000000000000000000000000000001", "image/jpeg")

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CONTENT_TYPE_MISMATCH")
}

// TestConfirmUpload_ContentTypeCaseInsensitive verifies ContentType comparison
// is case-insensitive ("TEXT/PLAIN" vs "text/plain" must pass) — the canonical
// MIME form is lowercase but real-world clients send mixed case.
func TestConfirmUpload_ContentTypeCaseInsensitive(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)

	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"),
		"00000000000000000000000000000001", "TEXT/PLAIN")

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err, "case-only difference must not be treated as a mismatch")
}

// TestConfirmUpload_ObjectACLViolation verifies ConfirmUpload rejects an
// upload whose cloud-side ACL is public-read when the session declared the
// file private (IsPublic=false). This catches clients that bypassed our
// STS Policy's LockObjectACL condition somehow (rogue credentials, bucket
// misconfiguration, etc.).
func TestConfirmUpload_ObjectACLViolation(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)
	// Default IsPublic is false — session is private.

	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)
	require.False(t, session.IsPublic, "session must be private for this test")

	// Simulate a rogue upload that ended up public-read on the cloud.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"),
		"00000000000000000000000000000001", session.ContentType)
	fp.SetObjectACL("uploads", session.ObjectKey, "public-read")

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OBJECT_ACL_VIOLATION")
}

// TestConfirmUpload_PrivateACLPasses verifies the inverse: an explicit
// "private" ACL on a private session must pass (not just be tolerated as
// "absent").
func TestConfirmUpload_PrivateACLPasses(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	verified, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)

	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"),
		"00000000000000000000000000000001", session.ContentType)
	fp.SetObjectACL("uploads", session.ObjectKey, "private")

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err, "explicit private ACL on private session must pass")
}

// --- CancelUpload integration tests ---

// TestGenerateUploadURL_ConfirmRoundTrip verifies that the pre-signed URL
// flow works end-to-end: GenerateUploadURL must mint a token tied to a real
// PENDING session row, and ConfirmUpload on that token must succeed.
//
// Regression coverage: an earlier revision minted the token without creating
// a session (no SessionID), and ConfirmUpload rejected SessionID==0 — so
// every non-instant upload through GenerateUploadURL was unconfirmable.
func TestGenerateUploadURL_ConfirmRoundTrip(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	// 1. Generate pre-signed upload URL (also mints the upload_token).
	resp, err := svc.GenerateUploadURL(ctx, &storagev1.GenerateUploadURLRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		Size:        4,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "gen.txt",
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant(), "no prior upload → not instant")
	require.NotEmpty(t, resp.GetUploadToken(), "token must be returned")
	require.NotEmpty(t, resp.GetUploadUrl(), "pre-signed URL must be returned")

	// 2. Decode token, confirm it carries a real SessionID and the session exists.
	verified, err := upload.VerifyTokenForTest(resp.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	require.NotZero(t, verified.SessionID, "token must carry a session id")
	session, err := dal.GetUploadSessionByID(ctx, svc.db, verified.SessionID)
	require.NoError(t, err)
	require.Equal(t, int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING), session.Status)

	// 3. Simulate client PUT to OSS using the object_key from the session.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"), "00000000000000000000000000000001", session.ContentType)

	// 4. Confirm — must succeed and create a File row.
	confirm, err := svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: resp.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)
	require.NotZero(t, confirm.GetFileId())
}

func TestCancelUpload_PendingThenCancel(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	// 1. Issue token (creates session in PENDING).
	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     100,
		Md5:         "00000000000000000000000000000001",
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)

	// 2. Cancel.
	_, err = svc.CancelUpload(ctx, &storagev1.CancelUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)

	// 3. Verify session is now CANCELLED.
	tok, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, tok.SessionID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CANCELLED)), session.Status)
}

func TestCancelUpload_AlreadyConfirmed(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	// 1. Issue + simulate upload + confirm.
	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     4,
		Md5:         "00000000000000000000000000000002",
		ContentType: "text/plain",
		Filename:    "b.txt",
	})
	require.NoError(t, err)

	// Resolve session's object_key (response only has prefix).
	tok, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, tok.SessionID)
	require.NoError(t, err)

	// Simulate OSS upload.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"), "00000000000000000000000000000002", session.ContentType)

	_, err = svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)

	// 2. Now cancel — should fail because session is CONFIRMED (not PENDING).
	_, err = svc.CancelUpload(ctx, &storagev1.CancelUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrUploadSessionNotPending.New()))
}

// --- UpdateMyFile tests ---

// seedFileForOwner inserts a StorageFile + StorageObject row directly for the
// given owner and returns the file ID. Used by file-mutation tests that need
// a pre-existing row without going through the upload flow.
func seedFileForOwner(t *testing.T, svc *StorageService, ownerType int32, ownerID int64, metadata map[string]string) int64 {
	t.Helper()
	ctx := context.Background()
	objID, err := svc.gid.NextID(ctx)
	require.NoError(t, err)
	obj := &models.StorageObject{
		ID: objID, Vendor: 3, Bucket: "uploads", ObjectKey: "uploads/seed",
		MD5: "seedmd5", Size: 4, ContentType: "text/plain", StorageClass: 1, RefCount: 1,
	}
	require.NoError(t, svc.db.Create(obj).Error)

	fileID, err := svc.gid.NextID(ctx)
	require.NoError(t, err)
	file := &models.StorageFile{
		ID:        fileID,
		OwnerType: ownerType,
		OwnerID:   ownerID,
		ObjectID:  objID,
		Filename:  "seed.txt",
		Metadata:  models.MapJSON(metadata),
	}
	require.NoError(t, dal.CreateFile(ctx, svc.db, file))
	return fileID
}

// seedPublicFileForOwner is like seedFileForOwner but marks the file as
// public (IsPublic=true). Used by GetCDNURL public-mode tests to satisfy
// the service-layer gate that requires file.is_public=true when the
// request asks for an unsigned CDN URL.
func seedPublicFileForOwner(t *testing.T, svc *StorageService, ownerType int32, ownerID int64) int64 {
	t.Helper()
	ctx := context.Background()
	objID, err := svc.gid.NextID(ctx)
	require.NoError(t, err)
	obj := &models.StorageObject{
		ID: objID, Vendor: 3, Bucket: "uploads", ObjectKey: "uploads/seed-public",
		MD5: "seedmd5pub", Size: 4, ContentType: "text/plain", StorageClass: 1, RefCount: 1,
	}
	require.NoError(t, svc.db.Create(obj).Error)

	fileID, err := svc.gid.NextID(ctx)
	require.NoError(t, err)
	file := &models.StorageFile{
		ID:        fileID,
		OwnerType: ownerType,
		OwnerID:   ownerID,
		ObjectID:  objID,
		Filename:  "seed-public.txt",
		IsPublic:  true,
	}
	require.NoError(t, dal.CreateFile(ctx, svc.db, file))
	return fileID
}

// TestUpdateMyFile_ClearMetadata verifies that setting clear_metadata=true in
// the request wipes the existing metadata map. Regression for the bug where
// proto3's empty-map default made it impossible to express "clear metadata"
// (the old len(metadata) > 0 guard meant an empty map was a no-op).
func TestUpdateMyFile_ClearMetadata(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, map[string]string{"k1": "v1", "k2": "v2"})

	clear := true
	_, err := svc.UpdateMyFile(ctx, &storagev1.UpdateMyFileRequest{
		FileId:        fileID,
		ClearMetadata: &clear,
		Owner:         &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)

	updated, err := dal.GetFileByID(ctx, svc.db, fileID)
	require.NoError(t, err)
	assert.Empty(t, updated.Metadata, "clear_metadata=true must empty the metadata map")
}

// TestUpdateMyFile_NoMetadataFieldUnchanged verifies that omitting both
// metadata entries and clear_metadata leaves the existing metadata intact.
// This is the proto3-friendly default: callers who only want to rename the
// file must not have their metadata wiped.
func TestUpdateMyFile_NoMetadataFieldUnchanged(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, map[string]string{"keep": "me"})

	newName := "renamed.txt"
	_, err := svc.UpdateMyFile(ctx, &storagev1.UpdateMyFileRequest{
		FileId:   fileID,
		Filename: &newName,
		Owner:    &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)

	updated, err := dal.GetFileByID(ctx, svc.db, fileID)
	require.NoError(t, err)
	assert.Equal(t, "renamed.txt", updated.Filename)
	assert.Equal(t, models.MapJSON{"keep": "me"}, updated.Metadata, "omitting metadata + clear_metadata must preserve existing entries")
}

// --- GetCDNURL service-layer tests ---
//
// These exercise the GenerateCDNURL RPC handler: file+object lookup, ownership
// check, FakeProvider-backed CDNURLGenerator dispatch, and TTL clamping
// (default/min/max). The fixture seeds a file owned by (owner_type=1, owner_id=100)
// whose object lives in the "uploads" bucket — the same bucket the FakeProvider
// is registered against, so CDNURLGeneratorForBucket resolves correctly.

// TestGetCDNURL_HappyPath verifies a signed CDN URL is returned with a future
// expiry and the FakeProvider's recognizable host.
func TestGetCDNURL_HappyPath(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
		Ttl:    durationpb.New(10 * time.Minute),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetUrl())
	assert.Contains(t, resp.GetUrl(), "cdn.test.example")
	assert.Greater(t, resp.GetExpiresAt(), time.Now().Unix())
}

// TestGetCDNURL_NotOwner verifies the ownership check: a caller that doesn't
// own the file gets FILE_NOT_FOUND (no existence leak).
func TestGetCDNURL_NotOwner(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	_, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 999},
		FileId: fileID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FILE_NOT_FOUND")
}

// TestGetCDNURL_DefaultTTL verifies a zero ttl falls back to default_ttl (1h).
func TestGetCDNURL_DefaultTTL(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
	})
	require.NoError(t, err)
	expected := time.Now().Add(time.Hour)
	actual := time.Unix(resp.GetExpiresAt(), 0)
	assert.WithinDuration(t, expected, actual, 2*time.Second)
}

// TestGetCDNURL_ClampTTL_BelowMin verifies a sub-min ttl is bumped to min_ttl.
func TestGetCDNURL_ClampTTL_BelowMin(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
		Ttl:    durationpb.New(time.Second), // below min_ttl (5m)
	})
	require.NoError(t, err)
	expected := time.Now().Add(5 * time.Minute)
	actual := time.Unix(resp.GetExpiresAt(), 0)
	assert.WithinDuration(t, expected, actual, 2*time.Second)
}

// TestGetCDNURL_ClampTTL_AboveMax verifies an over-max ttl is clamped to max_ttl.
func TestGetCDNURL_ClampTTL_AboveMax(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
		Ttl:    durationpb.New(7 * 24 * time.Hour), // 7 days, above max (24h)
	})
	require.NoError(t, err)
	expected := time.Now().Add(24 * time.Hour)
	actual := time.Unix(resp.GetExpiresAt(), 0)
	assert.WithinDuration(t, expected, actual, 2*time.Second)
}

// TestGetCDNURL_PublicMode_HappyPath verifies that public=true on a
// file marked IsPublic=true returns an unsigned URL (no fake_auth query
// param) and a zero expiry. The FakeProvider leaves the query string
// empty in public mode when ops is empty.
func TestGetCDNURL_PublicMode_HappyPath(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedPublicFileForOwner(t, svc, 1, 100)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
		Public: true,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetUrl())
	assert.Contains(t, resp.GetUrl(), "cdn.test.example")
	assert.NotContains(t, resp.GetUrl(), "fake_auth", "public URL has no auth param")
	assert.NotContains(t, resp.GetUrl(), "expires", "public URL has no expires param")
	assert.Equal(t, int64(0), resp.GetExpiresAt(), "public URL expiry is zero (permanent)")
}

// TestGetCDNURL_PublicMode_PrivateFileRejected verifies that public=true
// on a private file (IsPublic=false) is rejected with BAD_REQUEST,
// preventing accidentally exposing a private file via a public URL.
func TestGetCDNURL_PublicMode_PrivateFileRejected(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	// seedFileForOwner defaults IsPublic=false.
	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	_, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
		Public: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}

// TestGetCDNURL_Filename verifies that req.filename propagates through to
// the CDN URL as a response-content-disposition query param.
func TestGetCDNURL_Filename(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:    &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId:   fileID,
		Filename: proto.String("report.pdf"),
	})
	require.NoError(t, err)
	assert.Contains(t, resp.GetUrl(), "response-content-disposition=", "filename must surface as response-content-disposition query")
	assert.Contains(t, resp.GetUrl(), "report.pdf")
}

// TestGetCDNURL_NoFilename verifies that omitting filename does NOT add
// response-content-disposition — guards against accidental defaults.
func TestGetCDNURL_NoFilename(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	fileID := seedFileForOwner(t, svc, 1, 100, nil)

	resp, err := svc.file.GetCDNURL(ctx, &storagev1.GenerateCDNURLRequest{
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		FileId: fileID,
	})
	require.NoError(t, err)
	assert.NotContains(t, resp.GetUrl(), "response-content-disposition",
		"omitting filename must not add response-content-disposition")
}

// --- Task 10: end-to-end integration tests ---

// firstMD5Hex returns the MD5 hex digest of input. Used by the e2e tests below
// to fabricate MD5s that correspond to real upload bytes so ConfirmUpload's
// cross-check (ETag vs declared MD5) passes against the FakeProvider.
func firstMD5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestUpload_FullFlow exercises issue → upload → confirm → re-confirm (idempotent).
// Verifies: session transitions PENDING → CONFIRMED, single File created, quota
// reserved exactly once, second confirm returns same FileID.
func TestUpload_FullFlow(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	md5Hex := firstMD5Hex("data")

	// 1. Issue (creates PENDING session + caches STS).
	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     int64(len("data")),
		Md5:         md5Hex,
		ContentType: "text/plain",
		Filename:    "a.txt",
	})
	require.NoError(t, err)
	assert.False(t, creds.GetInstant())
	assert.NotEmpty(t, creds.GetUploadToken())

	// 2. Resolve object_key from session (response only has prefix).
	tok, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, tok.SessionID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING)), session.Status)

	// 3. Simulate OSS upload.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("data"), md5Hex, session.ContentType)

	// 4. Confirm (PENDING → CONFIRMED).
	r1, err := svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)
	fileID := r1.GetFileId()
	require.NotZero(t, fileID)

	// 5. Re-confirm: idempotent, returns same FileID.
	r2, err := svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		UploadToken: creds.GetUploadToken(),
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
	})
	require.NoError(t, err)
	assert.Equal(t, fileID, r2.GetFileId())

	// 6. Verify session is CONFIRMED.
	session2, err := dal.GetUploadSessionByID(ctx, svc.db, tok.SessionID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_CONFIRMED)), session2.Status)
	require.NotNil(t, session2.FileID)
	assert.Equal(t, fileID, *session2.FileID)

	// 7. Verify exactly one File row exists.
	files, err := dal.CountFilesByOwner(ctx, svc.db, 100, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), files)

	// 8. Verify quota consumed exactly once (4 bytes for "data").
	q, err := svc.quota.GetQuota(ctx, svc.db, 1, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(4), q.UsedBytes)
}

// TestUpload_GCFlow exercises issue → (no confirm) → GC → orphan cleanup.
// Verifies: session ends EXPIRED, OSS object deleted, no File created, quota untouched.
func TestUpload_GCFlow(t *testing.T) {
	svc, fp := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	md5Hex := firstMD5Hex("gc-data")

	// 1. Issue token (creates session).
	creds, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
		Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Bucket:      "uploads",
		MaxSize:     int64(len("gc-data")),
		Md5:         md5Hex,
		ContentType: "text/plain",
		Filename:    "gc.txt",
	})
	require.NoError(t, err)

	// 2. Resolve session object_key.
	tok, err := upload.VerifyTokenForTest(creds.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 100, 1)
	require.NoError(t, err)
	session, err := dal.GetUploadSessionByID(ctx, svc.db, tok.SessionID)
	require.NoError(t, err)

	// 3. Simulate orphan: client uploaded but didn't confirm.
	fakeUploadToProvider(fp, "uploads", session.ObjectKey, []byte("gc-data"), md5Hex, session.ContentType)
	require.True(t, fp.ObjectExists("uploads", session.ObjectKey))

	// 4. Force session expiry (manually set expires_at to past).
	past := time.Now().Add(-time.Minute)
	require.NoError(t, svc.db.WithContext(ctx).Model(&models.StorageUploadSession{}).
		Where("id = ?", session.ID).
		Update("expires_at", past).Error)

	// 5. Run reap.
	deleted, err := svc.upload.ReapExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	// 6. Verify session is EXPIRED.
	s, err := dal.GetUploadSessionByID(ctx, svc.db, session.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_EXPIRED)), s.Status)

	// 7. Verify OSS object deleted.
	assert.False(t, fp.ObjectExists("uploads", session.ObjectKey))

	// 8. Verify no File created.
	files, err := dal.CountFilesByOwner(ctx, svc.db, 100, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(0), files)

	// 9. Verify quota untouched.
	q, err := svc.quota.GetQuota(ctx, svc.db, 1, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(0), q.UsedBytes)
}

// TestMD5Dedup_VendorDiscrimination verifies that the same MD5 in different
// vendors (or different buckets within the same vendor) yields separate
// StorageObjects — they do NOT dedup. Fills a gap flagged in the Task 1 review
// (repo-level test coverage for vendor-aware dedup was missing).
func TestMD5Dedup_VendorDiscrimination(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()
	md5Hex := firstMD5Hex("same-content")

	// Insert object with vendor=1, bucket="uploads" via CreateOrGet (repo's
	// only insert path; the unique index (vendor,bucket,md5) scopes dedup).
	obj1 := &models.StorageObject{
		ID:           1001,
		Vendor:       1,
		Bucket:       "uploads",
		ObjectKey:    "uploads/" + md5Hex,
		MD5:          md5Hex,
		Size:         12,
		ContentType:  "text/plain",
		StorageClass: int32(storagev1.StorageClass_STORAGE_CLASS_STANDARD),
	}
	got, inserted, err := dal.CreateOrGetObject(ctx, svc.db, obj1)
	require.NoError(t, err)
	require.True(t, inserted, "first insert should create a new row")
	assert.Equal(t, int64(1001), got.ID)

	// Same MD5+bucket, same vendor → returns the existing one (no new row).
	existing, found, err := dal.FindObjectByVendorBucketMD5(ctx, svc.db, 1, "uploads", md5Hex)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(1001), existing.ID)

	// Same MD5+bucket, DIFFERENT vendor (2) → not found.
	_, found2, err := dal.FindObjectByVendorBucketMD5(ctx, svc.db, 2, "uploads", md5Hex)
	require.NoError(t, err)
	assert.False(t, found2, "different vendor should not match")

	// Same MD5+vendor, different bucket → not found.
	_, found3, err := dal.FindObjectByVendorBucketMD5(ctx, svc.db, 1, "other-bucket", md5Hex)
	require.NoError(t, err)
	assert.False(t, found3, "different bucket should not match")
}

// TestGetSTSCredential_ConcurrentDedup verifies that concurrent GetSTSCredential
// calls for the same (owner, md5, size) all observe the SAME session — the dedup
// lock (DedupLock) + FindPendingDedup fallback prevents the thundering
// herd. Fills a gap flagged in the Task 5 review.
//
// NOTE on miniredis: miniredis is single-threaded, so the SetNX-based lock
// serializes correctly here. Timing differs from a real Redis cluster (no real
// network latency), but the dedup invariant — "at most one PENDING session per
// (owner, md5, size)" — is what we actually assert.
func TestGetSTSCredential_ConcurrentDedup(t *testing.T) {
	svc, _ := setupServiceWithFakeObjectProvider(t)
	ctx := context.Background()

	md5Hex := firstMD5Hex("concurrent")
	const N = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	responses := make([]*storagev1.GetSTSCredentialResponse, N)
	errs := make([]error, N)

	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := svc.GetSTSCredential(ctx, &storagev1.GetSTSCredentialRequest{
				Owner:       &storagev1.Owner{OwnerType: 1, OwnerId: 200},
				Bucket:      "uploads",
				MaxSize:     int64(len("concurrent")),
				Md5:         md5Hex,
				ContentType: "text/plain",
				Filename:    "x.txt",
			})
			responses[i] = resp
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	// All N calls should succeed.
	for i, err := range errs {
		require.NoError(t, err, "call %d failed", i)
	}

	// All N tokens should reference the SAME session.
	firstSessionID := int64(0)
	for i, resp := range responses {
		tok, err := upload.VerifyTokenForTest(resp.GetUploadToken(), svc.cfg.Storage.UploadTokenSecret, 200, 1)
		require.NoError(t, err, "call %d token verify", i)
		if i == 0 {
			firstSessionID = tok.SessionID
			require.NotZero(t, firstSessionID)
		} else {
			assert.Equal(t, firstSessionID, tok.SessionID,
				"call %d should reuse session from call 0", i)
		}
	}

	// Verify only one PENDING session exists for this (owner, md5, size).
	var count int64
	require.NoError(t, svc.db.WithContext(ctx).
		Model(&models.StorageUploadSession{}).
		Where("owner_type = ? AND owner_id = ? AND md5 = ? AND status = ?",
			1, 200, md5Hex, int32(int32(storagev1.UploadSessionStatus_UPLOAD_SESSION_STATUS_PENDING))).
		Count(&count).Error)
	assert.Equal(t, int64(1), count, "exactly one PENDING session should exist for (owner, md5, size)")
}
