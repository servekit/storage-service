# Admin Stubs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement 5 admin gRPC methods (AdminListFiles, AdminGetFile, AdminDeleteFile, AdminListProviders, AdminListBuckets) and remove 2 unused Create RPCs from proto.

**Architecture:** File management stubs (list/get/delete) follow existing user-facing patterns but remove owner restrictions. Provider/Bucket listing reads from the in-memory Registry populated by YAML config. Proto is updated to add provider/bucket filter fields to AdminListFilesRequest and remove AdminCreateProvider/AdminCreateBucket RPCs.

**Tech Stack:** Go, gRPC, GORM, Protocol Buffers, buf

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `api/proto/storage/v1/storage.proto` | Modify | Remove 2 RPCs + 2 messages, add provider/bucket filters |
| `gen/storage/v1/*.go` | Regenerate | `buf generate` |
| `internal/store/repository/file_repo.go` | Modify | Add `GetByID`, `ListAll`, `AdminListFilesFilter` |
| `internal/provider/registry.go` | Modify | Add `providerConfigs` field, `AllProviders()`, `AllBuckets()` |
| `internal/service/admin.go` | Modify | Add 5 private method implementations |
| `internal/service/helpers.go` | Modify | Add `buildAdminFileInfo` |
| `internal/service/service.go` | Modify | Update gRPC stubs (remove 2, update 5) |

---

### Task 1: Proto — Remove Create RPCs, add provider/bucket filters

**Files:**
- Modify: `api/proto/storage/v1/storage.proto`

- [ ] **Step 1: Edit proto file**

Remove `AdminCreateProvider` RPC (line 127):
```
  // AdminCreateProvider creates a storage provider (admin only).
  rpc AdminCreateProvider(AdminCreateProviderRequest) returns (ProviderInfo);
```

Remove `AdminCreateBucket` RPC (line 131):
```
  // AdminCreateBucket creates a bucket (admin only).
  rpc AdminCreateBucket(AdminCreateBucketRequest) returns (BucketInfo);
```

Remove `AdminCreateProviderRequest` message (lines 432-439):
```
message AdminCreateProviderRequest {
  string name = 1 [(buf.validate.field).string = {min_len: 1}];
  ProviderType type = 2;
  string endpoint = 3;
  string region = 4;
  string access_key = 5 [(buf.validate.field).string = {min_len: 1}];
  string secret_key = 6 [(buf.validate.field).string = {min_len: 1}];
}
```

Remove `AdminCreateBucketRequest` message (lines 452-457):
```
message AdminCreateBucketRequest {
  string name = 1 [(buf.validate.field).string = {min_len: 1}];
  string provider = 2 [(buf.validate.field).string = {min_len: 1}];
  string key_prefix = 3;
  BucketACL acl = 4;
}
```

Add `provider` and `bucket` fields to `AdminListFilesRequest` (after line 9):
```protobuf
  string provider = 10;
  string bucket = 11;
```

- [ ] **Step 2: Regenerate proto code**

Run: `buf generate`

- [ ] **Step 3: Verify build**

Run: `go build ./...`

Expected: compile errors in `service.go` for removed RPC stubs — that's OK, Task 5 fixes them.

- [ ] **Step 4: Commit**

```bash
git add api/ gen/
git commit -m "refactor: remove AdminCreateProvider/AdminCreateBucket RPCs, add provider/bucket filters to AdminListFilesRequest"
```

---

### Task 2: Repository — FileRepo new methods

**Files:**
- Modify: `internal/store/repository/file_repo.go`

- [ ] **Step 1: Add `AdminListFilesFilter` type**

Add after the existing `ListFilesFilter` struct (around line 37):

```go
// AdminListFilesFilter defines filtering and pagination options for admin file listing.
// All filter fields are optional — zero values mean "no filter".
type AdminListFilesFilter struct {
	OwnerType         int32
	OwnerID           int64
	PathPrefix        string
	Extension         string
	ContentTypePrefix string
	Provider          string
	Bucket            string
	OrderBy           storagev1.SortField
	Descending        bool
	dbx.Pagination
}
```

- [ ] **Step 2: Add `GetByID` method**

Add after `GetByIDAndOwner`:

```go
// GetByID retrieves a file by ID without owner check (admin use).
func (r *FileRepo) GetByID(ctx context.Context, id int64) (*models.File, error) {
	f, err := gorm.G[models.File](r.db).
		Where(generated.File.ID.Eq(id)).
		Where(generated.File.DeletedAt.IsNull()).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrFileNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &f, nil
}
```

- [ ] **Step 3: Add `ListAll` method**

Add after `ListByOwner`:

```go
// ListAll returns a paginated list of all files with optional filters (admin use).
func (r *FileRepo) ListAll(ctx context.Context, filter AdminListFilesFilter) ([]models.File, int, error) {
	q := gorm.G[models.File](r.db).Where(generated.File.DeletedAt.IsNull())

	if filter.OwnerType > 0 {
		q = q.Where(generated.File.OwnerType.Eq(filter.OwnerType))
	}
	if filter.OwnerID > 0 {
		q = q.Where(generated.File.OwnerID.Eq(filter.OwnerID))
	}
	if filter.PathPrefix != "" {
		q = q.Where(generated.File.FilePath.Like(filter.PathPrefix + "%"))
	}
	if filter.Extension != "" {
		q = q.Where(generated.File.Filename.Like("%." + filter.Extension))
	}

	// Content type, provider, bucket filters require StorageObject data.
	needObjectJoin := filter.ContentTypePrefix != "" || filter.Provider != "" || filter.Bucket != ""

	if needObjectJoin {
		objQ := gorm.G[models.StorageObject](r.db).Where(generated.StorageObject.DeletedAt.IsNull())
		if filter.ContentTypePrefix != "" {
			objQ = objQ.Where(generated.StorageObject.ContentType.Like(filter.ContentTypePrefix + "%"))
		}
		if filter.Provider != "" {
			objQ = objQ.Where(generated.StorageObject.Provider.Eq(filter.Provider))
		}
		if filter.Bucket != "" {
			objQ = objQ.Where(generated.StorageObject.Bucket.Eq(filter.Bucket))
		}

		objects, err := objQ.Find(ctx)
		if err != nil {
			return nil, 0, xcodes.ErrInternal.Wrapf(err, "find objects for admin filter")
		}
		if len(objects) == 0 {
			return nil, 0, nil
		}
		objectIDs := make([]int64, len(objects))
		for i, o := range objects {
			objectIDs[i] = o.ID
		}
		q = q.Where(generated.File.ObjectID.In(objectIDs...))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count files (admin)")
	}

	switch filter.OrderBy {
	case storagev1.SortField_SORT_FIELD_FILENAME:
		if filter.Descending {
			q = q.Order(generated.File.Filename.Desc())
		} else {
			q = q.Order(generated.File.Filename)
		}
	case storagev1.SortField_SORT_FIELD_SIZE:
		// Size lives on StorageObject; fall back to created_at ordering.
		if filter.Descending {
			q = q.Order(generated.File.CreatedAt.Desc())
		} else {
			q = q.Order(generated.File.CreatedAt)
		}
	default:
		if filter.Descending {
			q = q.Order(generated.File.CreatedAt.Desc())
		} else {
			q = q.Order(generated.File.CreatedAt)
		}
	}

	pg := filter.Normalize()

	if pg.AfterID > 0 {
		q = q.Where(generated.File.ID.Lt(pg.AfterID))
	}

	files, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list files (admin)")
	}

	return files, int(total), nil
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./internal/store/repository/`

- [ ] **Step 5: Commit**

```bash
git add internal/store/repository/file_repo.go
git commit -m "feat: add GetByID and ListAll methods to FileRepo for admin operations"
```

---

### Task 3: Registry — Add AllProviders and AllBuckets

**Files:**
- Modify: `internal/provider/registry.go`

- [ ] **Step 1: Add `providerConfigs` field to Registry**

Add `providerConfigs` field to the Registry struct (after `bucketProviders` line):

```go
	providerConfigs map[string]config.ProviderConfig
```

In `NewRegistry`, add initialization to the struct literal (after `bucketProviders` line):

```go
		providerConfigs: make(map[string]config.ProviderConfig),
```

In the `for _, pc := range providers` loop body, add this line after `r.providers[pc.Name] = p`:

```go
		r.providerConfigs[pc.Name] = pc
```

- [ ] **Step 2: Add `ProviderEntry` and `AllProviders` method**

```go
// ProviderEntry holds provider metadata for listing.
type ProviderEntry struct {
	Name     string
	Type     string // raw config string, e.g. "aliyun_oss"
	Endpoint string
	Region   string
}

// AllProviders returns metadata for all registered providers.
func (r *Registry) AllProviders() []ProviderEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]ProviderEntry, 0, len(r.providerConfigs))
	for _, pc := range r.providerConfigs {
		entries = append(entries, ProviderEntry{
			Name:     pc.Name,
			Type:     pc.Type,
			Endpoint: pc.Endpoint,
			Region:   pc.Region,
		})
	}
	return entries
}
```

- [ ] **Step 3: Add `BucketEntry` and `AllBuckets` method**

```go
// BucketEntry holds bucket metadata for listing.
type BucketEntry struct {
	Name      string
	Provider  string
	KeyPrefix string
	ACL       string
}

// AllBuckets returns metadata for all registered buckets.
func (r *Registry) AllBuckets() []BucketEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entries := make([]BucketEntry, 0, len(r.buckets))
	for name, bc := range r.buckets {
		entries = append(entries, BucketEntry{
			Name:      name,
			Provider:  r.bucketProviders[name],
			KeyPrefix: bc.KeyPrefix,
			ACL:       bc.ACL,
		})
	}
	return entries
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./internal/provider/`

- [ ] **Step 5: Commit**

```bash
git add internal/provider/registry.go
git commit -m "feat: add AllProviders and AllBuckets methods to provider Registry"
```

---

### Task 4: Helpers — Add buildAdminFileInfo

**Files:**
- Modify: `internal/service/helpers.go`

- [ ] **Step 1: Add `buildAdminFileInfo` function**

Add after `buildUserFileInfo`:

```go
// buildAdminFileInfo converts a File model and its StorageObject into an
// AdminFileInfo proto message.
func buildAdminFileInfo(file *models.File, obj *models.StorageObject) *storagev1.AdminFileInfo {
	if obj == nil {
		obj = &models.StorageObject{}
	}

	metadata := make(map[string]string)
	for k, v := range file.Metadata {
		metadata[k] = v
	}

	return &storagev1.AdminFileInfo{
		Id:          file.ID,
		OwnerType:   ownerTypeToProto(file.OwnerType),
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
		Provider:    obj.Provider,
		Bucket:      obj.Bucket,
		ObjectKey:   obj.ObjectKey,
		CreatedAt:   file.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   file.UpdatedAt.Format(time.RFC3339),
	}
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./internal/service/`

- [ ] **Step 3: Commit**

```bash
git add internal/service/helpers.go
git commit -m "feat: add buildAdminFileInfo helper for admin file info conversion"
```

---

### Task 5: Service — Update gRPC stubs in service.go

**Files:**
- Modify: `internal/service/service.go`

- [ ] **Step 1: Remove AdminCreateProvider and AdminCreateBucket stubs**

Delete these four methods (lines 171-185):

```go
func (StorageService) AdminCreateProvider(_ context.Context, _ *storagev1.AdminCreateProviderRequest) (*storagev1.ProviderInfo, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (StorageService) AdminListProviders(_ context.Context, _ *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (StorageService) AdminCreateBucket(_ context.Context, _ *storagev1.AdminCreateBucketRequest) (*storagev1.BucketInfo, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}

func (StorageService) AdminListBuckets(_ context.Context, _ *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "not implemented")
}
```

- [ ] **Step 2: Replace 5 admin stubs with delegations**

Replace the `// Admin (stubs)` section (lines 157-165) with:

```go
// Admin (file management)

func (s *StorageService) AdminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	return s.adminListFiles(ctx, req)
}

func (s *StorageService) AdminGetFile(ctx context.Context, req *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	return s.adminGetFile(ctx, req)
}

func (s *StorageService) AdminDeleteFile(ctx context.Context, req *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	return s.adminDeleteFile(ctx, req)
}

// Admin (provider/bucket)

func (s *StorageService) AdminListProviders(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	return s.adminListProviders(ctx, req)
}

func (s *StorageService) AdminListBuckets(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	return s.adminListBuckets(ctx, req)
}
```

- [ ] **Step 3: Clean up unused imports**

Remove `"google.golang.org/grpc/codes"` and `"google.golang.org/grpc/status"` if they are no longer used. Keep `"google.golang.org/protobuf/types/known/emptypb"`.

- [ ] **Step 4: Verify build**

Run: `go build ./internal/service/`

Expected: compile errors for undefined methods (adminListFiles, etc.) — Task 6 implements them.

- [ ] **Step 5: Commit**

```bash
git add internal/service/service.go
git commit -m "refactor: update admin gRPC stubs, remove CreateProvider/CreateBucket"
```

---

### Task 6: Service — Implement admin methods

**Files:**
- Modify: `internal/service/admin.go`

- [ ] **Step 1: Add `adminListFiles`**

Add to `admin.go`:

```go
func (s *StorageService) adminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	filter := repository.AdminListFilesFilter{
		OwnerType:         int32(req.GetOwnerType()),
		OwnerID:           req.GetOwnerId(),
		PathPrefix:        req.GetPathPrefix(),
		Extension:         req.GetExtension(),
		ContentTypePrefix: req.GetContentTypePrefix(),
		Provider:          req.GetProvider(),
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

	files, total, err := s.fileRepo.ListAll(ctx, filter)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	pg := filter.Normalize()

	objectIDs := make([]int64, 0, len(files))
	for _, f := range files {
		objectIDs = append(objectIDs, f.ObjectID)
	}
	objectsMap, err := s.objectRepo.BatchGetByIDs(ctx, objectIDs)
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
```

- [ ] **Step 2: Add `adminGetFile`**

```go
func (s *StorageService) adminGetFile(ctx context.Context, req *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	f, err := s.fileRepo.GetByID(ctx, req.GetFileId())
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := s.objectRepo.GetByID(ctx, f.ObjectID)
	if err != nil {
		return nil, xcodes.ErrObjectNotFound.Wrap(err)
	}

	return buildAdminFileInfo(f, obj), nil
}
```

- [ ] **Step 3: Add `adminDeleteFile`**

```go
func (s *StorageService) adminDeleteFile(ctx context.Context, req *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	f, err := s.fileRepo.GetByID(ctx, req.GetFileId())
	if err != nil {
		return nil, xcodes.ErrFileNotFound.Wrap(err)
	}

	obj, err := s.objectRepo.GetByID(ctx, f.ObjectID)
	if err != nil {
		return nil, xcodes.ErrObjectNotFound.Wrap(err)
	}

	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txFileRepo := repository.NewFileRepo(tx)
		txObjRepo := repository.NewObjectRepo(tx)

		if delErr := txFileRepo.SoftDelete(ctx, f.ID); delErr != nil {
			return delErr
		}
		if refErr := txObjRepo.DecrRefCount(ctx, obj.ID); refErr != nil {
			return refErr
		}
		if releaseErr := s.release(ctx, tx, f.OwnerType, f.OwnerID, obj.Size); releaseErr != nil {
			return releaseErr
		}
		return nil
	})
	if txErr != nil {
		return nil, xcodes.ErrInternal.Wrap(txErr)
	}

	return &emptypb.Empty{}, nil
}
```

- [ ] **Step 4: Add `adminListProviders`**

```go
func (s *StorageService) adminListProviders(ctx context.Context, _ *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	entries := s.registry.AllProviders()

	providers := make([]*storagev1.ProviderInfo, 0, len(entries))
	for _, e := range entries {
		pt, ok := storagev1.ProviderType_value[e.Type]
		if !ok {
			pt = int32(storagev1.ProviderType_PROVIDER_TYPE_UNSPECIFIED)
		}
		providers = append(providers, &storagev1.ProviderInfo{
			Name:     e.Name,
			Type:     storagev1.ProviderType(pt),
			Endpoint: e.Endpoint,
			Region:   e.Region,
		})
	}

	return &storagev1.AdminListProvidersResponse{Providers: providers}, nil
}
```

- [ ] **Step 5: Add `adminListBuckets`**

```go
func (s *StorageService) adminListBuckets(ctx context.Context, _ *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	entries := s.registry.AllBuckets()

	buckets := make([]*storagev1.BucketInfo, 0, len(entries))
	for _, e := range entries {
		buckets = append(buckets, &storagev1.BucketInfo{
			Name:      e.Name,
			Provider:  e.Provider,
			KeyPrefix: e.KeyPrefix,
			Acl:       aclStringToProto(e.ACL),
		})
	}

	return &storagev1.AdminListBucketsResponse{Buckets: buckets}, nil
}
```

- [ ] **Step 6: Add `aclStringToProto` helper to `helpers.go`**

```go
func aclStringToProto(acl string) storagev1.BucketACL {
	switch acl {
	case "private":
		return storagev1.BucketACL_BUCKET_ACL_PRIVATE
	case "public_read":
		return storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ
	case "public_read_write":
		return storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ_WRITE
	default:
		return storagev1.BucketACL_BUCKET_ACL_UNSPECIFIED
	}
}
```

- [ ] **Step 7: Update imports in admin.go**

Ensure `admin.go` imports include:

```go
import (
	"context"
	"fmt"
	"strconv"

	"github.com/servekit/go-common/dbx"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/store/repository"
	"storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)
```

- [ ] **Step 8: Verify build**

Run: `go build ./...`

Expected: clean build, no errors.

- [ ] **Step 9: Commit**

```bash
git add internal/service/admin.go internal/service/helpers.go
git commit -m "feat: implement AdminListFiles, AdminGetFile, AdminDeleteFile, AdminListProviders, AdminListBuckets"
```

---

### Task 7: Final verification

- [ ] **Step 1: Run full build**

Run: `go build ./...`

- [ ] **Step 2: Run existing tests**

Run: `go test -race ./...`

Expected: all tests pass (existing test coverage should not be affected).

- [ ] **Step 3: Run lint**

Run: `golangci-lint run ./...`

Expected: no new warnings.

- [ ] **Step 4: Final commit if any fixes needed**

```bash
git add -A
git commit -m "chore: lint and test fixes"
```
