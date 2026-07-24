package admin

import (
	"context"
	"testing"
	"time"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- buildAdminFileInfo tests ---

func TestBuildAdminFileInfo(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 11, 12, 30, 0, 0, time.UTC)
	file := &models.StorageFile{
		ID:          1001,
		OwnerType:   1, // user
		OwnerID:     42,
		ObjectID:    2001,
		Filename:    "report.pdf",
		FilePath:    "/docs/2026/report.pdf",
		Description: "Annual report",
		Metadata: models.MapJSON{
			"source": "upload",
			"tag":    "finance",
		},
		IsPublic:  true,
		CreatedAt: now,
		UpdatedAt: now.Add(1 * time.Hour),
	}

	obj := &models.StorageObject{
		ID:           2001,
		Vendor:       int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE),
		Bucket:       "test-bucket",
		ObjectKey:    "ab/cdef1234567890",
		MD5:          "cdef1234567890",
		Size:         2048,
		ContentType:  "application/pdf",
		Extension:    ".pdf",
		ETag:         "etag-abc",
		StorageClass: 1,
		RefCount:     3,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	info := buildAdminFileInfo(file, obj)

	// Verify file fields
	assert.Equal(t, int64(1001), info.Id)
	assert.Equal(t, storagev1.OwnerType_OWNER_TYPE_USER, info.OwnerType)
	assert.Equal(t, int64(42), info.OwnerId)
	assert.Equal(t, "report.pdf", info.Filename)
	assert.Equal(t, "/docs/2026/report.pdf", info.FilePath)
	assert.Equal(t, "Annual report", info.Description)
	assert.True(t, info.IsPublic)
	assert.Equal(t, int64(2001), info.ObjectId)

	// Verify object fields
	assert.Equal(t, int64(2048), info.Size)
	assert.Equal(t, "application/pdf", info.ContentType)
	assert.Equal(t, ".pdf", info.Extension)
	assert.Equal(t, "cdef1234567890", info.Md5)
	assert.Equal(t, "VENDOR_S3_COMPATIBLE", info.Provider)
	assert.Equal(t, "test-bucket", info.Bucket)
	assert.Equal(t, "ab/cdef1234567890", info.ObjectKey)

	// Verify timestamps
	assert.Equal(t, now.Format(time.RFC3339), info.CreatedAt)
	assert.Equal(t, now.Add(1*time.Hour).Format(time.RFC3339), info.UpdatedAt)

	// Verify metadata
	require.Len(t, info.Metadata, 2)
	assert.Equal(t, "upload", info.Metadata["source"])
	assert.Equal(t, "finance", info.Metadata["tag"])
}

func TestBuildAdminFileInfo_NilObject(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	file := &models.StorageFile{
		ID:          500,
		OwnerType:   2, // system
		OwnerID:     1,
		ObjectID:    600,
		Filename:    "config.yaml",
		FilePath:    "/system/config.yaml",
		Description: "",
		Metadata:    nil,
		IsPublic:    false,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// obj is nil — should not panic, should use zero-value defaults
	info := buildAdminFileInfo(file, nil)

	assert.Equal(t, int64(500), info.Id)
	assert.Equal(t, storagev1.OwnerType_OWNER_TYPE_SYSTEM, info.OwnerType)
	assert.Equal(t, int64(1), info.OwnerId)
	assert.Equal(t, "config.yaml", info.Filename)
	assert.Equal(t, "/system/config.yaml", info.FilePath)
	assert.Equal(t, int64(600), info.ObjectId)

	// Object fields should be zero-valued
	assert.Equal(t, int64(0), info.Size)
	assert.Equal(t, "", info.ContentType)
	assert.Equal(t, "", info.Extension)
	assert.Equal(t, "", info.Md5)
	assert.Equal(t, "", info.Provider)
	assert.Equal(t, "", info.Bucket)
	assert.Equal(t, "", info.ObjectKey)

	// Metadata should be empty map, not nil
	assert.NotNil(t, info.Metadata)
	assert.Empty(t, info.Metadata)
}

// --- AdminListProviders / AdminListBuckets tests ---

func TestAdminListProviders(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t)
	svc := New(&Deps{Registry: registry})

	resp, err := svc.AdminListProviders(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Providers, 2, "should list 2 providers")

	byName := make(map[string]*storagev1.ProviderInfo, len(resp.Providers))
	for _, p := range resp.Providers {
		byName[p.Name] = p
	}

	p1, ok := byName["minio-local"]
	require.True(t, ok, "minio-local provider should exist")
	assert.Equal(t, storagev1.Vendor_VENDOR_S3_COMPATIBLE, p1.Vendor)
	assert.Equal(t, "http://localhost:9000", p1.Endpoint)
	assert.Equal(t, "us-east-1", p1.Region)

	p2, ok := byName["wasabi-backup"]
	require.True(t, ok, "wasabi-backup provider should exist")
	assert.Equal(t, storagev1.Vendor_VENDOR_S3_COMPATIBLE, p2.Vendor)
	assert.Equal(t, "https://s3.wasabisys.com", p2.Endpoint)
	assert.Equal(t, "us-east-1", p2.Region)
}

func TestAdminListProviders_EmptyRegistry(t *testing.T) {
	t.Parallel()

	registry, err := storage.NewRegistry(nil)
	require.NoError(t, err)
	svc := New(&Deps{Registry: registry})

	resp, err := svc.AdminListProviders(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.Providers)
}

func TestAdminListBuckets(t *testing.T) {
	t.Parallel()

	registry := newTestRegistry(t)
	svc := New(&Deps{Registry: registry})

	resp, err := svc.AdminListBuckets(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, resp.Buckets, 3, "should list 3 buckets")

	byName := make(map[string]*storagev1.BucketInfo, len(resp.Buckets))
	for _, b := range resp.Buckets {
		byName[b.Name] = b
	}

	b1, ok := byName["uploads"]
	require.True(t, ok, "uploads bucket should exist")
	assert.Equal(t, "minio-local", b1.Provider)
	assert.Equal(t, "uploads/", b1.KeyPrefix)
	assert.Equal(t, storagev1.BucketACL_BUCKET_ACL_PRIVATE, b1.Acl)

	b2, ok := byName["assets"]
	require.True(t, ok, "assets bucket should exist")
	assert.Equal(t, "minio-local", b2.Provider)
	assert.Equal(t, "assets/", b2.KeyPrefix)
	assert.Equal(t, storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ, b2.Acl)

	b3, ok := byName["backups"]
	require.True(t, ok, "backups bucket should exist")
	assert.Equal(t, "wasabi-backup", b3.Provider)
	assert.Equal(t, "backup/", b3.KeyPrefix)
	assert.Equal(t, storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ_WRITE, b3.Acl)
}

func TestAdminListBuckets_EmptyRegistry(t *testing.T) {
	t.Parallel()

	registry, err := storage.NewRegistry(nil)
	require.NoError(t, err)
	svc := New(&Deps{Registry: registry})

	resp, err := svc.AdminListBuckets(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.Buckets)
}

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
