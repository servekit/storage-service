# Admin Stubs Implementation Design

## Scope

Implement 5 admin stub methods, remove 2 Create RPCs from proto:

| Method | Action |
|--------|--------|
| `AdminListFiles` | Full implementation |
| `AdminGetFile` | Full implementation |
| `AdminDeleteFile` | Full implementation |
| `AdminListProviders` | Read from Registry |
| `AdminListBuckets` | Read from Registry |
| `AdminCreateProvider` | Remove from proto |
| `AdminCreateBucket` | Remove from proto |

## Proto Changes

`api/proto/storage/v1/storage.proto`:

1. Remove `AdminCreateProvider` RPC (line 127)
2. Remove `AdminCreateBucket` RPC (line 131)
3. Remove `AdminCreateProviderRequest` message (lines 432-439)
4. Remove `AdminCreateBucketRequest` message (lines 452-457)
5. Add `string provider = 10` and `string bucket = 11` to `AdminListFilesRequest`

After changes, regenerate `gen/` via `buf generate`.

## Repository Layer

### FileRepo — new methods

**`GetByID(ctx, id int64) (*File, error)`**
- Single record lookup by ID, no owner check
- Returns `xcodes.ErrFileNotFound` if not found or soft-deleted

**`ListAll(ctx, filter AdminListFilesFilter) ([]File, int, error)`**
- New `AdminListFilesFilter` struct:
  ```
  OwnerType        int32   (0 = all)
  OwnerID          int64   (0 = all)
  PathPrefix       string
  Extension        string
  ContentTypePrefix string
  Provider         string  (0 = all, filters via JOIN on storage_objects)
  Bucket           string  (0 = all, filters via JOIN on storage_objects)
  OrderBy          SortField
  Descending       bool
  Pagination       dbx.Pagination
  ```
- When `Provider` or `Bucket` is set, JOIN `storage_objects` on `files.object_id = storage_objects.id`
- Owner fields are optional (zero = no filter)
- Reuse existing cursor-based pagination pattern from `ListByOwner`

### Registry — new methods

**`AllProviders() []ProviderEntry`**
- Returns provider metadata (name, type, endpoint, region) from config
- Need a new struct or return `[]config.ProviderConfig` with a method that maps Type string to `ProviderType` enum

**`AllBuckets() []BucketEntry`**
- Returns all buckets with their provider association and ACL
- Each entry: name, provider name, key_prefix, acl

Implementation: Registry already stores `providers` map and `buckets`/`bucketProviders` maps. Add two methods that iterate and return structured data. Need to also store `ProviderConfig` metadata (type, endpoint, region) in the Registry for `AllProviders()` — currently only the `Provider` interface is stored, not the config metadata.

### Registry enhancement

Add `providerConfigs map[string]config.ProviderConfig` to Registry, populated in `NewRegistry`. This preserves the config metadata needed for `AllProviders()`.

## Service Layer

### `adminListFiles`

```
1. Build AdminListFilesFilter from request fields
2. Parse page_token (snowflake ID cursor)
3. Call fileRepo.ListAll(ctx, filter)
4. Batch-fetch StorageObjects via objectRepo.BatchGetByIDs
5. Convert to []AdminFileInfo (new helper: buildAdminFileInfo)
6. Handle pagination (TrimPage + next_page_token)
7. Return AdminListFilesResponse
```

### `adminGetFile`

```
1. Call fileRepo.GetByID(ctx, fileID)
2. Fetch StorageObject via objectRepo.GetByID
3. Convert to AdminFileInfo via buildAdminFileInfo
4. Return
```

### `adminDeleteFile`

```
1. Call fileRepo.GetByID(ctx, fileID) — get file with owner info
2. Fetch StorageObject via objectRepo.GetByID — get size
3. Transaction:
   a. txFileRepo.SoftDelete(ctx, fileID)
   b. txObjRepo.DecrRefCount(ctx, objectID)
   c. s.release(ctx, tx, ownerType, ownerID, object.Size)
4. Return empty
```

Pattern mirrors `deleteMyFile` but uses `GetByID` (no owner check).

### `adminListProviders`

```
1. Call registry.AllProviders()
2. Map each ProviderConfig to ProviderInfo proto (Type string → ProviderType enum)
3. Return AdminListProvidersResponse
```

### `adminListBuckets`

```
1. Call registry.AllBuckets()
2. Map each bucket to BucketInfo proto (ACL string → BucketACL enum)
3. Return AdminListBucketsResponse
```

## Helper: `buildAdminFileInfo`

New helper in `helpers.go`:

```go
func buildAdminFileInfo(file *models.File, obj *models.StorageObject) *storagev1.AdminFileInfo
```

Combines `File` + `StorageObject` into `AdminFileInfo`, including:
- File metadata: id, filename, file_path, description, metadata, is_public
- Owner: owner_type, owner_id
- Object info: object_id, size, content_type, extension, md5, provider, bucket, object_key
- Timestamps: created_at, updated_at

## Service wiring (`service.go`)

- Remove `AdminCreateProvider` and `AdminCreateBucket` stubs
- Replace `AdminListFiles`, `AdminGetFile`, `AdminDeleteFile`, `AdminListProviders`, `AdminListBuckets` stubs with delegations to private methods

## Files Changed

| File | Change |
|------|--------|
| `api/proto/storage/v1/storage.proto` | Remove 2 RPCs + 2 messages, add provider/bucket filters |
| `gen/storage/v1/*.go` | Regenerated |
| `internal/store/repository/file_repo.go` | Add `GetByID`, `ListAll`, `AdminListFilesFilter` |
| `internal/provider/registry.go` | Add `providerConfigs` field, `AllProviders()`, `AllBuckets()` |
| `internal/service/admin.go` | Add `adminListFiles`, `adminGetFile`, `adminDeleteFile`, `adminListProviders`, `adminListBuckets` |
| `internal/service/helpers.go` | Add `buildAdminFileInfo` |
| `internal/service/service.go` | Update gRPC stubs |
