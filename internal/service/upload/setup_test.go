package upload

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/fake"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/internal/thirdcall/gid_service"
	"github.com/servekit/storage-service/pkg/config"

	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const testSecret = "test-secret-key-12345"

// seqGID is a gid_service.GIDService returning sequential IDs with no external
// dependency. Mirrors the one in the parent service_test.go.
type seqGID struct {
	counter int64
}

func (g *seqGID) NextID(_ context.Context) (int64, error) {
	return atomic.AddInt64(&g.counter, 1), nil
}

func (g *seqGID) Close() error { return nil }

// Compile-time assertion that *seqGID satisfies gid_service.GIDService.
var _ gid_service.GIDService = (*seqGID)(nil)

// noopHost is a minimal upload.Host used by reap tests that don't assert on quota
// or audit recording. ReapExpiredSessions never calls CheckQuota/Reserve, and
// these tests don't inspect audit rows, so all three methods are no-ops.
type noopHost struct{}

func (noopHost) CheckQuota(context.Context, *gorm.DB, int32, int64, int64) error { return nil }
func (noopHost) Reserve(context.Context, *gorm.DB, int32, int64, int64) error    { return nil }
func (noopHost) RecordOutcome(context.Context, AuditEvent, error)                {}

// setupUploadServiceWithFakeProvider builds an upload.Service wired to a real
// Postgres testcontainer, miniredis, and a FakeProvider-backed registry. Returns
// the service, the fake provider, and the underlying *gorm.DB (GC tests seed and
// inspect session rows directly via the DB handle).
func setupUploadServiceWithFakeProvider(t *testing.T, host Host) (*Service, *fake.FakeProvider, *gorm.DB) {
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
	registry, err := storage.NewRegistryWithProvider(providerCfg, fp, nil)
	require.NoError(t, err)

	gid := &seqGID{}
	cfg := &config.Config{
		Storage: &config.StorageConfig{
			UploadTokenTTL:    30 * time.Minute,
			UploadTokenSecret: testSecret,
			DefaultQuotaBytes: 1 << 30,
			DefaultBucket:     "uploads",
			UploadGC:          &config.UploadGCConfig{BatchSize: 100},
			Batch:             &config.BatchConfig{MaxSize: 100, Concurrency: 4},
		},
	}

	svc := New(&Deps{
		DB:        db,
		Registry:  registry,
		GID:       gid,
		Cfg:       cfg,
		Redis:     rdb,
		STS:       &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour},
		DedupLock: NewDedupLock(rdb, &config.LockConfig{}),
		Host:      host,
	})
	return svc, fp, db
}
