package file

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/provider/storage/fake"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestBuildUserFileInfo_full verifies every field is mapped correctly from a
// populated StorageFile + StorageObject pair, including the RFC3339 timestamp
// formatting and owner_type enum conversion.
func TestBuildUserFileInfo_full(t *testing.T) {
	created := time.Date(2026, 6, 20, 10, 30, 0, 0, time.UTC)
	updated := time.Date(2026, 6, 20, 11, 0, 0, 0, time.UTC)

	file := &models.StorageFile{
		ID:          701,
		OwnerType:   2, // OWNER_TYPE_SYSTEM
		OwnerID:     99,
		ObjectID:    42,
		Filename:    "report.pdf",
		FilePath:    "docs/2026/report.pdf",
		Description: "Q2 report",
		Metadata:    models.MapJSON{"source": "upload", "lang": "en"},
		IsPublic:    true,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}
	obj := &models.StorageObject{
		Size:        4096,
		ContentType: "application/pdf",
		Extension:   "pdf",
		MD5:         "abcdef0123456789abcdef0123456789",
	}

	got := buildUserFileInfo(file, obj)

	assert.Equal(t, int64(701), got.Id)
	assert.Equal(t, "report.pdf", got.Filename)
	assert.Equal(t, "docs/2026/report.pdf", got.FilePath)
	assert.Equal(t, "Q2 report", got.Description)
	assert.True(t, got.IsPublic)
	assert.Equal(t, storagev1.OwnerType_OWNER_TYPE_SYSTEM, got.OwnerType)
	assert.Equal(t, int64(4096), got.Size)
	assert.Equal(t, "application/pdf", got.ContentType)
	assert.Equal(t, "pdf", got.Extension)
	assert.Equal(t, "abcdef0123456789abcdef0123456789", got.Md5)
	assert.Equal(t, created.Format(time.RFC3339), got.CreatedAt)
	assert.Equal(t, updated.Format(time.RFC3339), got.UpdatedAt)
	// Metadata fully copied.
	assert.Equal(t, map[string]string{"source": "upload", "lang": "en"}, got.Metadata)
}

// TestBuildUserFileInfo_nilObject verifies a nil object does not panic and
// yields zero-value size/content_type/extension/md5 while file-sourced fields
// are still populated.
func TestBuildUserFileInfo_nilObject(t *testing.T) {
	file := &models.StorageFile{
		ID:        702,
		OwnerType: 1,
		Filename:  "empty.txt",
		CreatedAt: time.Time{}.Add(time.Second),
		UpdatedAt: time.Time{}.Add(time.Second),
	}

	got := buildUserFileInfo(file, nil)

	require.NotNil(t, got)
	assert.Equal(t, int64(702), got.Id)
	assert.Equal(t, "empty.txt", got.Filename)
	// Object-derived fields must be zero, not nil-dereference panics.
	assert.Zero(t, got.Size)
	assert.Empty(t, got.ContentType)
	assert.Empty(t, got.Extension)
	assert.Empty(t, got.Md5)
}

// TestBuildUserFileInfo_metadataCopy verifies the metadata map is copied, not
// shared by reference: mutating the returned proto's metadata must not affect
// the source model's map.
func TestBuildUserFileInfo_metadataCopy(t *testing.T) {
	src := models.MapJSON{"k": "v"}
	file := &models.StorageFile{
		ID:       703,
		Metadata: src,
	}
	obj := &models.StorageObject{}

	got := buildUserFileInfo(file, obj)

	// Mutate the proto's metadata; the source must be unchanged.
	got.Metadata["k"] = "mutated"
	got.Metadata["injected"] = "x"
	assert.Equal(t, models.MapJSON{"k": "v"}, src, "source metadata must be isolated from proto mutations")
	assert.Equal(t, "mutated", got.Metadata["k"])
}

// TestBuildUserFileInfo_emptyMetadata verifies an empty source metadata map
// produces a non-nil, empty proto map (proto3 maps default to empty, never nil).
func TestBuildUserFileInfo_emptyMetadata(t *testing.T) {
	file := &models.StorageFile{ID: 704, Metadata: models.MapJSON{}}
	obj := &models.StorageObject{}

	got := buildUserFileInfo(file, obj)

	require.NotNil(t, got.Metadata)
	assert.Empty(t, got.Metadata)
}

// TestBuildUserFileInfo_nilMetadata verifies a nil source metadata map is
// handled without panic and yields an empty (non-nil) proto map.
func TestBuildUserFileInfo_nilMetadata(t *testing.T) {
	file := &models.StorageFile{ID: 705} // Metadata is nil
	obj := &models.StorageObject{}

	got := buildUserFileInfo(file, obj)

	require.NotNil(t, got.Metadata)
	assert.Empty(t, got.Metadata)
}

// TestListMyFilesPaged_Validation verifies page and page_size bounds.
// Uses a nil-db Service: input validation runs before any DB call.
func TestListMyFilesPaged_Validation(t *testing.T) {
	svc := &Service{} // nil db is fine — validation fails first

	cases := []struct {
		name     string
		page     int32
		pageSize int32
	}{
		{"page zero", 0, 10},
		{"page negative", -1, 10},
		{"page_size zero", 1, 0},
		{"page_size negative", 1, -1},
		{"page_size over max", 1, 101},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ListMyFilesPaged(context.Background(), &storagev1.ListMyFilesPagedRequest{
				Page: tc.page, PageSize: tc.pageSize,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "BAD_REQUEST")
		})
	}
}

// TestListMyFilesPaged_SuccessPath exercises the full success path with real
// DB rows: verifies total_count, total_pages, and has_more are computed
// correctly across pages.
func TestListMyFilesPaged_SuccessPath(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}

	// Seed 25 files via direct DAL (avoid circular dep on file package's
	// private helpers). Filenames f00..f24 so filename-asc ordering is
	// deterministic.
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		f := models.StorageFile{
			OwnerType: 1,
			OwnerID:   100,
			ObjectID:  int64(1000 + i),
			Filename:  fmt.Sprintf("f%02d", i),
		}
		if err := db.Create(&f).Error; err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	svc := &Service{db: db}

	// Page 2 of 3, 10 per page.
	resp, err := svc.ListMyFilesPaged(ctx, &storagev1.ListMyFilesPagedRequest{
		Owner:    &storagev1.Owner{OwnerType: 1, OwnerId: 100},
		Page:     2,
		PageSize: 10,
		OrderBy:  storagev1.SortField_SORT_FIELD_FILENAME,
	})
	require.NoError(t, err)
	require.Len(t, resp.Files, 10)
	assert.Equal(t, int64(25), resp.TotalCount)
	assert.Equal(t, int32(2), resp.Page)
	assert.Equal(t, int32(3), resp.TotalPages)
	assert.True(t, resp.HasMore)

	// Spot-check ordering: page 2 should be f10..f19 in filename-asc order.
	assert.Equal(t, "f10", resp.Files[0].Filename)
	assert.Equal(t, "f19", resp.Files[9].Filename)
}

// setupFileServiceWithFakeProvider wires a file.Service backed by a real
// Postgres testcontainer and a FakeProvider registry with one private and
// one public bucket. Returns the service, fake provider, and DB.
func setupFileServiceWithFakeProvider(t *testing.T) (*Service, *fake.FakeProvider) {
	t.Helper()

	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}

	fp := fake.NewFakeProvider()

	providerCfg := &config.ProviderConfig{
		Name:     "fake-local",
		Vendor:   "VENDOR_S3_COMPATIBLE",
		Endpoint: "http://fake-endpoint",
		Region:   "us-east-1",
		Buckets: []*config.BucketConfig{
			{Name: "uploads", KeyPrefix: "uploads/", ACL: "private"},
			{Name: "assets", KeyPrefix: "assets/", ACL: "public_read"},
		},
	}
	registry, err := storage.NewRegistryWithProvider(providerCfg, fp, nil)
	require.NoError(t, err)

	svc := New(&Deps{DB: db, Registry: registry})
	return svc, fp
}

// TestGenerateDownloadURL_PublicObject verifies that when the object's bucket
// is public_read (object.IsPublic=true), the returned URL is unsigned — no
// signature query params, just the bare https://fake.example/<bucket>/<key>
// with the ?public=true marker the fake appends for assertion.
func TestGenerateDownloadURL_PublicObject(t *testing.T) {
	svc, _ := setupFileServiceWithFakeProvider(t)
	ctx := context.Background()

	// Seed object marked public + file referencing it.
	obj := &models.StorageObject{
		Bucket:    "assets",
		ObjectKey: "assets/ab/abcd1234.dat",
		MD5:       "abcd1234abcd1234abcd1234abcd1234",
		Size:      123,
		IsPublic:  true,
		Vendor:    int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE),
	}
	if err := svc.db.Create(obj).Error; err != nil {
		t.Fatalf("seed object: %v", err)
	}
	file := &models.StorageFile{
		OwnerType: 1,
		OwnerID:   42,
		ObjectID:  obj.ID,
		Filename:  "logo.png",
		IsPublic:  obj.IsPublic, // mirror
	}
	if err := svc.db.Create(file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}

	resp, err := svc.GenerateDownloadURL(ctx, &storagev1.GenerateDownloadURLRequest{
		FileId: file.ID,
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 42},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.DownloadUrl)

	// The fake provider appends "public=true" when WithPublic() is passed.
	// Public URL: bare host with no signature query params. Match loosely
	// (with leading ? or &) so the assertion survives query ordering.
	assert.Contains(t, resp.DownloadUrl, "public=true",
		"public download URL should carry the WithPublic marker")
	assert.NotContains(t, resp.DownloadUrl, "Signature=",
		"public download URL should be unsigned")
	assert.True(t, strings.HasPrefix(resp.DownloadUrl, "https://fake.example/assets/"),
		"public download URL should be the bare bucket/key path; got %q", resp.DownloadUrl)
}

// TestGenerateDownloadURL_PrivateObject verifies that a private object
// (IsPublic=false) returns the normal presigned URL — no ?public=true marker.
func TestGenerateDownloadURL_PrivateObject(t *testing.T) {
	svc, _ := setupFileServiceWithFakeProvider(t)
	ctx := context.Background()

	obj := &models.StorageObject{
		Bucket:    "uploads",
		ObjectKey: "uploads/ef/ef012345.dat",
		MD5:       "ef012345ef012345ef012345ef012345",
		Size:      456,
		IsPublic:  false,
		Vendor:    int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE),
	}
	if err := svc.db.Create(obj).Error; err != nil {
		t.Fatalf("seed object: %v", err)
	}
	file := &models.StorageFile{
		OwnerType: 1,
		OwnerID:   42,
		ObjectID:  obj.ID,
		Filename:  "secret.pdf",
		IsPublic:  false,
	}
	if err := svc.db.Create(file).Error; err != nil {
		t.Fatalf("seed file: %v", err)
	}

	resp, err := svc.GenerateDownloadURL(ctx, &storagev1.GenerateDownloadURLRequest{
		FileId: file.ID,
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 42},
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.DownloadUrl)

	// Private URL has no public marker.
	assert.False(t, strings.Contains(resp.DownloadUrl, "public=true"),
		"private download URL should not carry the public marker; got %q", resp.DownloadUrl)
	assert.True(t, strings.HasPrefix(resp.DownloadUrl, "https://fake.example/uploads/"),
		"private download URL should still target the bucket/key path; got %q", resp.DownloadUrl)
}

// TestGenerateDownloadURL_FilenameFallback verifies that omitting filename
// falls back to the stored uf.Filename. The FakeProvider surfaces filename
// as a query marker so we can assert it was passed through.
func TestGenerateDownloadURL_FilenameFallback(t *testing.T) {
	svc, _ := setupFileServiceWithFakeProvider(t)
	ctx := context.Background()

	obj := &models.StorageObject{
		Bucket:    "uploads",
		ObjectKey: "uploads/aa/aaaa.dat",
		MD5:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Size:      1,
		IsPublic:  false,
		Vendor:    int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE),
	}
	require.NoError(t, svc.db.Create(obj).Error)
	file := &models.StorageFile{
		OwnerType: 1, OwnerID: 42, ObjectID: obj.ID,
		Filename: "original.txt",
	}
	require.NoError(t, svc.db.Create(file).Error)

	resp, err := svc.GenerateDownloadURL(ctx, &storagev1.GenerateDownloadURLRequest{
		FileId: file.ID,
		Owner:  &storagev1.Owner{OwnerType: 1, OwnerId: 42},
	})
	require.NoError(t, err)
	assert.Contains(t, resp.DownloadUrl, "filename=original.txt",
		"omitting filename must fall back to uf.Filename; got %q", resp.DownloadUrl)
}

// TestGenerateDownloadURL_FilenameOverride verifies that req.filename
// overrides the stored uf.Filename when non-empty.
func TestGenerateDownloadURL_FilenameOverride(t *testing.T) {
	svc, _ := setupFileServiceWithFakeProvider(t)
	ctx := context.Background()

	obj := &models.StorageObject{
		Bucket:    "uploads",
		ObjectKey: "uploads/bb/bbbb.dat",
		MD5:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Size:      1,
		IsPublic:  false,
		Vendor:    int32(storagev1.Vendor_VENDOR_S3_COMPATIBLE),
	}
	require.NoError(t, svc.db.Create(obj).Error)
	file := &models.StorageFile{
		OwnerType: 1, OwnerID: 42, ObjectID: obj.ID,
		Filename: "stored-name.txt",
	}
	require.NoError(t, svc.db.Create(file).Error)

	resp, err := svc.GenerateDownloadURL(ctx, &storagev1.GenerateDownloadURLRequest{
		FileId:   file.ID,
		Owner:    &storagev1.Owner{OwnerType: 1, OwnerId: 42},
		Filename: proto.String("save-as.txt"),
	})
	require.NoError(t, err)
	assert.Contains(t, resp.DownloadUrl, "filename=save-as.txt",
		"req.filename must override uf.Filename; got %q", resp.DownloadUrl)
	assert.NotContains(t, resp.DownloadUrl, "filename=stored-name.txt",
		"uf.Filename must not leak through when req.filename is set; got %q", resp.DownloadUrl)
}
