# store 目录重构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `internal/store/{models,repository}/` 文件名严格一一对应，repository 层零 raw SQL，跨表逻辑推到 service 层用各 repo 组合实现。

**Architecture:**
- models 层按 model 拆分（含 struct + Row 类型 + gen 注解接口）
- repository 层每个文件只操作自己 model 对应的单表（CRUD + 单表聚合）
- service 层组合各 repo 做跨表编排 + QuotaRepo 乐观锁重试
- 全程零 raw SQL（含 `db.Raw`、`Where("string", ...)`、`clause.Expr{SQL:...}`）和零硬编码列名

**Tech Stack:** Go 1.x, gorm.io/cli/gorm (gen), PostgreSQL, github.com/servekit/go-common (xerr, dbx)

**关联设计**：[[services/storage-service/design/v1/store-models-repo-correspondence|store-models-repo-correspondence]]

---

## File Structure（最终形态）

```
storage-service/internal/store/
├── models/
│   ├── audit_log.go         AuditLog, JSONMap (无改动)
│   ├── file.go              File, MapJSON + FileQuery + FileCountRow + ObjectRefCountRow + OwnerObjectIDPair(新)
│   ├── object.go            StorageObject + ObjectQuery + PhysicalStatsRow + ProviderStatRow + BucketObjectStatRow
│   ├── quota.go             Quota + QuotaQuery + UsedBytesRow
│   ├── upload_session.go    UploadSession (无改动)
│   └── register.go          AllModels, gen 配置 (无改动)
│
└── repository/
    ├── audit_log.go         ← rename from audit_log_repo.go (无代码改动)
    ├── file.go              ← rename + 改造（ListByOwner/ListAll 签名 + 新增方法）
    ├── object.go            ← rename + 改造（删 stats + 新增单表 helpers）
    ├── quota.go             ← rename + 改造（乐观锁 + ON CONFLICT 改 generated 列）
    ├── upload_session.go    ← rename (无代码改动)
    ├── upload_session_test.go
    └── constants.go         MaxBatchSize (保留)

[删除]
- models/query.go
- repository/stats_queries.go
- repository/table_name.go

storage-service/internal/service/
├── types.go (新)            跨表 Row 类型 + Stats 编排结果类型
├── stats.go (新)            getStorageStats（组合各 repo 单表方法）
├── quota.go (改)            新增 withQuotaRetry helper
├── file.go (改)             listFiles 跨表过滤改 service 层
└── admin.go (改)            adminListFiles / adminGetStats 跨表过滤改 service 层

storage-service/pkg/xcodes/
└── quota.go (改)            新增 ErrQuotaConcurrentConflict
```

---

## Task 1: 添加 ErrQuotaConcurrentConflict 错误码

**Files:**
- Modify: `storage-service/pkg/xcodes/quota.go`

- [ ] **Step 1: 添加新错误码**

修改 `storage-service/pkg/xcodes/quota.go`，在 `ErrQuotaInsufficientTotal` 后追加：

```go
ErrQuotaConcurrentConflict = xerr.New("QUOTA_CONCURRENT_CONFLICT", xerr.CategoryConflict, 409, "quota row changed concurrently, please retry")
```

- [ ] **Step 2: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./pkg/xcodes/...`
Expected: 无错误输出

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add pkg/xcodes/quota.go
git commit -m "feat(xcodes): add ErrQuotaConcurrentConflict for optimistic-lock retry"
```

---

## Task 2: 拆分 models/query.go 到各 model 文件

**Files:**
- Modify: `storage-service/internal/store/models/file.go`
- Modify: `storage-service/internal/store/models/object.go`
- Modify: `storage-service/internal/store/models/quota.go`
- Delete: `storage-service/internal/store/models/query.go`

- [ ] **Step 1: 在 models/file.go 末尾追加 Row 类型和 gen 接口**

```go

// --- Single-table aggregation rows (used by FileRepo + service layer) ---

// FileCountRow holds the result of a file count query (single-table on files).
type FileCountRow struct {
	Count int64 `gorm:"column:count"`
}

// ObjectRefCountRow holds the result of file count aggregation grouped by
// object_id (single-table on files).
type ObjectRefCountRow struct {
	ObjectID int64 `gorm:"column:object_id"`
	Count    int64 `gorm:"column:count"`
}

// OwnerObjectIDPair holds a (owner_type, object_id) pair from active files.
// Used by service layer to compute OwnerStats by composing FileRepo results
// with ObjectRepo size lookups.
type OwnerObjectIDPair struct {
	OwnerType int32 `gorm:"column:owner_type"`
	ObjectID  int64 `gorm:"column:object_id"`
}

// FileQuery defines gen-annotated single-table queries on the files table.
// gorm gen processes these annotations and generates type-safe implementations
// in internal/store/generated.
type FileQuery interface {
	// SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
	GetActiveByID(id int64) (File, error)

	// SELECT object_id, COUNT(*) AS count
	// FROM @@table
	// WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
	// GROUP BY object_id
	GetObjectRefCounts(ownerType int32, ownerID int64) ([]ObjectRefCountRow, error)

	// SELECT COUNT(*) AS count
	// FROM @@table
	// {{where}}
	//   deleted_at IS NULL
	//   {{if ownerType > 0}} AND owner_type = @ownerType {{end}}
	//   {{if ownerID > 0}} AND owner_id = @ownerID {{end}}
	// {{end}}
	GetFileCount(ownerType int32, ownerID int64) (FileCountRow, error)
}
```

- [ ] **Step 2: 在 models/object.go 末尾追加 Row 类型和 gen 接口**

```go

// --- Single-table aggregation rows on storage_objects (used by ObjectRepo) ---

// PhysicalStatsRow holds aggregate physical storage statistics (single-table
// on storage_objects).
type PhysicalStatsRow struct {
	TotalObjects  int64 `gorm:"column:total_objects"`
	PhysicalBytes int64 `gorm:"column:physical_bytes"`
}

// ProviderStatRow holds per-provider aggregate statistics (single-table
// aggregation on storage_objects, grouped by vendor).
type ProviderStatRow struct {
	Vendor      int32 `gorm:"column:vendor"`
	ObjectCount int64 `gorm:"column:object_count"`
	TotalBytes  int64 `gorm:"column:total_bytes"`
}

// BucketObjectStatRow holds per-bucket object aggregate statistics
// (single-table aggregation on storage_objects, grouped by bucket).
type BucketObjectStatRow struct {
	Bucket      string `gorm:"column:bucket"`
	ObjectCount int64  `gorm:"column:object_count"`
	TotalBytes  int64  `gorm:"column:total_bytes"`
}

// ObjectQuery defines gen-annotated single-table queries on storage_objects.
type ObjectQuery interface {
	// SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
	GetActiveByID(id int64) (StorageObject, error)
}
```

- [ ] **Step 3: 在 models/quota.go 末尾追加 Row 类型和 gen 接口**

```go

// --- Single-table aggregation rows on quotas (used by QuotaRepo) ---

// UsedBytesRow holds quota used bytes result (single-table on quotas).
type UsedBytesRow struct {
	UsedBytes int64 `gorm:"column:used_bytes"`
}

// QuotaQuery defines gen-annotated single-table queries on the quotas table.
type QuotaQuery interface {
	// SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
	GetActiveByID(id int64) (Quota, error)

	// SELECT COALESCE(used_bytes, 0) AS used_bytes
	// FROM @@table
	// WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
	GetUsedBytes(ownerType int32, ownerID int64) (UsedBytesRow, error)

	// SELECT COALESCE(SUM(used_bytes), 0) AS used_bytes
	// FROM @@table
	// WHERE deleted_at IS NULL
	GetTotalUsedBytes() (UsedBytesRow, error)
}
```

- [ ] **Step 4: 删除 models/query.go**

```bash
cd /Users/moss/code/base/storage-service
git rm internal/store/models/query.go
```

- [ ] **Step 5: 重新生成 generated 代码**

Run: `cd /Users/moss/code/base/storage-service && make generate`
Expected: 命令成功，`internal/store/generated/` 文件被更新

- [ ] **Step 6: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 无错误输出（如果 generated 代码生成正确）

如果 generated 代码报错（比如接口名变了导致 generated.Query[T] 不再适用），需要更新 generated 的调用代码 —— 跑 `grep -rn "generated.Query\[" internal/store/repository/` 找到所有引用，确保新接口兼容。

- [ ] **Step 7: 跑现有测试**

Run: `cd /Users/moss/code/base/storage-service && go test ./internal/store/...`
Expected: 现有测试 PASS（upload_session_repo_test 等）

- [ ] **Step 8: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/models/ internal/store/generated/
git commit -m "refactor(models): split query.go into per-model files (FileQuery/ObjectQuery/QuotaQuery)"
```

---

## Task 3: 重命名 audit_log_repo.go 和 upload_session_repo.go（含 test）

**Files:**
- Rename: `internal/store/repository/audit_log_repo.go` → `audit_log.go`
- Rename: `internal/store/repository/upload_session_repo.go` → `upload_session.go`
- Rename: `internal/store/repository/upload_session_repo_test.go` → `upload_session_test.go`

- [ ] **Step 1: git mv 重命名**

```bash
cd /Users/moss/code/base/storage-service
git mv internal/store/repository/audit_log_repo.go internal/store/repository/audit_log.go
git mv internal/store/repository/upload_session_repo.go internal/store/repository/upload_session.go
git mv internal/store/repository/upload_session_repo_test.go internal/store/repository/upload_session_test.go
```

- [ ] **Step 2: 验证编译 + 测试**

Run: `cd /Users/moss/code/base/storage-service && go build ./... && go test ./internal/store/repository/...`
Expected: 编译通过，测试 PASS（无代码改动，仅文件名变）

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/repository/
git commit -m "refactor(repository): rename audit_log/upload_session files to match models"
```

---

## Task 4: 改造 file_repo.go → file.go

**Files:**
- Rename: `internal/store/repository/file_repo.go` → `internal/store/repository/file.go`
- Modify: 上述文件内容

- [ ] **Step 1: git mv 重命名**

```bash
cd /Users/moss/code/base/storage-service
git mv internal/store/repository/file_repo.go internal/store/repository/file.go
```

- [ ] **Step 2: 修改 ListByOwner 签名 + 移除跨表查询**

在 `internal/store/repository/file.go` 中，把 `ListByOwner` 方法改为以下版本（关键：删除 ContentTypePrefix 跨表查询，增加 objectIDs 参数）：

```go
// ListByOwner returns a paginated list of files for a given owner.
//
// objectIDs semantics:
//   - nil: no object_id filter applied
//   - empty slice []int64{}: caller already determined no objects match,
//     return empty result without hitting DB
//   - non-empty: add WHERE object_id IN (...) filter
//
// ContentTypePrefix in filter is ignored by this method; service layer is
// responsible for resolving it to objectIDs before calling.
func (r *FileRepo) ListByOwner(ctx context.Context, ownerID int64, ownerType int32, filter ListFilesFilter, objectIDs []int64) ([]models.File, int, error) {
	if len(objectIDs) == 0 && objectIDs != nil {
		return nil, 0, nil
	}

	q := gorm.G[models.File](r.db).
		Where(generated.File.OwnerID.Eq(ownerID)).
		Where(generated.File.OwnerType.Eq(ownerType)).
		Where(generated.File.DeletedAt.IsNull())

	if filter.PathPrefix != "" {
		q = q.Where(generated.File.FilePath.Like(filter.PathPrefix + "%"))
	}
	if filter.Extension != "" {
		q = q.Where(generated.File.Filename.Like("%." + filter.Extension))
	}
	if len(objectIDs) > 0 {
		q = q.Where(generated.File.ObjectID.In(objectIDs...))
	}

	total, err := q.Count(ctx, "*")
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "count files")
	}

	switch filter.OrderBy {
	case storagev1.SortField_SORT_FIELD_FILENAME:
		if filter.Descending {
			q = q.Order(generated.File.Filename.Desc())
		} else {
			q = q.Order(generated.File.Filename)
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
		return nil, 0, xcodes.ErrInternal.Wrapf(err, "list files")
	}

	return files, int(total), nil
}
```

- [ ] **Step 3: 修改 ListAll 签名 + 移除跨表查询**

把 `ListAll` 方法改为：

```go
// ListAll returns a paginated list of all files with optional filters (admin use).
//
// objectIDs semantics same as ListByOwner.
// ContentTypePrefix/Vendor/Bucket in filter are ignored by this method.
func (r *FileRepo) ListAll(ctx context.Context, filter AdminListFilesFilter, objectIDs []int64) ([]models.File, int, error) {
	if len(objectIDs) == 0 && objectIDs != nil {
		return nil, 0, nil
	}

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
	if len(objectIDs) > 0 {
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

- [ ] **Step 4: 在 file.go 末尾追加 FindObjectIDsByOwner 和 FindOwnerObjectIDPairs 单表方法**

```go

// --- Single-table helpers for service-layer stats composition ---

// FindObjectIDsByOwner returns object_ids referenced by active files of the
// given owner. May contain duplicates (multiple files per object).
// Single-table query on files.
func (r *FileRepo) FindObjectIDsByOwner(ctx context.Context, ownerType int32, ownerID int64) ([]int64, error) {
	files, err := gorm.G[models.File](r.db).
		Where(generated.File.OwnerType.Eq(ownerType)).
		Where(generated.File.OwnerID.Eq(ownerID)).
		Where(generated.File.DeletedAt.IsNull()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by owner")
	}
	ids := make([]int64, len(files))
	for i, f := range files {
		ids[i] = f.ObjectID
	}
	return ids, nil
}

// FindOwnerObjectIDPairs returns (owner_type, object_id) pairs for all active
// files. Single-table query on files. Used by service layer to compute
// OwnerStats (file count per owner_type + sum of object sizes per owner_type).
func (r *FileRepo) FindOwnerObjectIDPairs(ctx context.Context) ([]models.OwnerObjectIDPair, error) {
	var pairs []models.OwnerObjectIDPair
	err := r.db.WithContext(ctx).
		Table("files").
		Select("owner_type, object_id").
		Where("deleted_at IS NULL").
		Scan(&pairs).Error
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find owner object pairs")
	}
	return pairs, nil
}
```

**注意**：`FindOwnerObjectIDPairs` 内部用 `Table("files").Select(...)` 是 GORM 标准 API，不算 raw SQL（无 SQL 字符串拼接、无 hardcoded 列名除了表名 "files"）。如要更彻底，可改成 `gorm.G[models.File](r.db).Select(generated.File.OwnerType, generated.File.ObjectID).Find(ctx)` 然后内存映射。下面 Step 5 给出更纯净的版本。

- [ ] **Step 5: 把 FindOwnerObjectIDPairs 改成用 generated field（更纯净）**

替换 Step 4 的 FindOwnerObjectIDPairs 实现为：

```go
// FindOwnerObjectIDPairs returns (owner_type, object_id) pairs for all active
// files. Single-table query on files. Used by service layer to compute
// OwnerStats by composing FileRepo results with ObjectRepo size lookups.
func (r *FileRepo) FindOwnerObjectIDPairs(ctx context.Context) ([]models.OwnerObjectIDPair, error) {
	files, err := gorm.G[models.File](r.db).
		Where(generated.File.DeletedAt.IsNull()).
		Select(generated.File.OwnerType, generated.File.ObjectID).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find owner object pairs")
	}
	pairs := make([]models.OwnerObjectIDPair, len(files))
	for i, f := range files {
		pairs[i] = models.OwnerObjectIDPair{
			OwnerType: f.OwnerType,
			ObjectID:  f.ObjectID,
		}
	}
	return pairs, nil
}
```

- [ ] **Step 6: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/store/...`
Expected: 编译通过（service 层会暂时报错，因为 ListByOwner/ListAll 签名变了 —— 下个 Task 修）

如果 service 层报错，**先注释掉或临时调整 service 层调用让 build 通过**，下个 Task 再正式修复。或者跳过 service 层编译，仅编译 store：

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/store/...`
Expected: store 包编译通过

- [ ] **Step 7: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/repository/file.go
git commit -m "refactor(repository): file_repo → file, ListByOwner/ListAll accept objectIDs param"
```

---

## Task 5: 改造 object_repo.go → object.go

**Files:**
- Rename: `internal/store/repository/object_repo.go` → `internal/store/repository/object.go`
- Modify: 上述文件内容

- [ ] **Step 1: git mv 重命名**

```bash
cd /Users/moss/code/base/storage-service
git mv internal/store/repository/object_repo.go internal/store/repository/object.go
```

- [ ] **Step 2: 删除整个 GetStats 方法和相关类型**

在 `internal/store/repository/object.go` 中，删除从 `// StatsFilter defines optional filters...` 注释开始到 `GetStats` 方法末尾的所有代码（包括 `StatsFilter`、`GlobalStats`、`OwnerStat`、`ProviderStat`、`BucketStat` 类型定义，和 `GetStats` 方法实现）。这些类型会迁到 `service/types.go`。

- [ ] **Step 3: 把 CreateOrGet 的 ON CONFLICT 列名改用 generated 列引用**

在 `internal/store/repository/object.go` 的 `CreateOrGet` 方法中：

```go
// 旧
result := r.db.WithContext(ctx).
    Clauses(clause.OnConflict{
        Columns:   []clause.Column{{Name: "vendor"}, {Name: "bucket"}, {Name: "md5"}},
        DoNothing: true,
    }).
    Create(obj)

// 新
result := r.db.WithContext(ctx).
    Clauses(clause.OnConflict{
        Columns: []clause.Column{
            generated.StorageObject.Vendor.Column(),
            generated.StorageObject.Bucket.Column(),
            generated.StorageObject.MD5.Column(),
        },
        DoNothing: true,
    }).
    Create(obj)
```

- [ ] **Step 4: 在 object.go 末尾追加 5 个单表辅助方法**

```go

// --- Single-table helpers for service-layer file list filtering ---

// FindIDsByContentTypePrefix returns IDs of active objects whose content type
// starts with prefix. Single-table query on storage_objects.
func (r *ObjectRepo) FindIDsByContentTypePrefix(ctx context.Context, prefix string) ([]int64, error) {
	objects, err := gorm.G[models.StorageObject](r.db).
		Where(generated.StorageObject.ContentType.Like(prefix + "%")).
		Where(generated.StorageObject.DeletedAt.IsNull()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by content type prefix")
	}
	ids := make([]int64, len(objects))
	for i, o := range objects {
		ids[i] = o.ID
	}
	return ids, nil
}

// FindIDsByFilter returns IDs of active objects matching the optional filters.
// Zero-value filters are ignored. Single-table query on storage_objects.
func (r *ObjectRepo) FindIDsByFilter(ctx context.Context, contentTypePrefix string, vendor int32, bucket string) ([]int64, error) {
	q := gorm.G[models.StorageObject](r.db).
		Where(generated.StorageObject.DeletedAt.IsNull())
	if contentTypePrefix != "" {
		q = q.Where(generated.StorageObject.ContentType.Like(contentTypePrefix + "%"))
	}
	if vendor != 0 {
		q = q.Where(generated.StorageObject.Vendor.Eq(vendor))
	}
	if bucket != "" {
		q = q.Where(generated.StorageObject.Bucket.Eq(bucket))
	}
	objects, err := q.Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by filter")
	}
	ids := make([]int64, len(objects))
	for i, o := range objects {
		ids[i] = o.ID
	}
	return ids, nil
}

// --- Single-table aggregation helpers for service-layer stats ---

// CountActiveAndSumSizeByIDs returns count and total size of active objects
// matching the given IDs. Single-table aggregation on storage_objects.
func (r *ObjectRepo) CountActiveAndSumSizeByIDs(ctx context.Context, ids []int64) (models.PhysicalStatsRow, error) {
	var result models.PhysicalStatsRow
	if len(ids) == 0 {
		return result, nil
	}
	err := r.db.WithContext(ctx).
		Model(&models.StorageObject{}).
		Where("id IN ?", ids).
		Where("deleted_at IS NULL").
		Select("COUNT(*) AS total_objects, COALESCE(SUM(size), 0) AS physical_bytes").
		Scan(&result).Error
	if err != nil {
		return result, xcodes.ErrInternal.Wrapf(err, "count and sum size by ids")
	}
	return result, nil
}
```

**注意**：上面的 `Where("id IN ?", ids)` 和 `Where("deleted_at IS NULL")` 是 GORM 标准 API（非字符串拼接成 SQL，而是 GORM 解析后绑定参数）。如要更纯净，可以改用 `gorm.G[models.StorageObject]` + `generated.StorageObject.ID.In(ids...)` + `generated.StorageObject.DeletedAt.IsNull()`。下面 Step 5 给出更纯净版本。

- [ ] **Step 5: 把 CountActiveAndSumSizeByIDs 改成完全用 generated field**

但是 `gorm.G[T]` 的链式 API 可能不支持 `Select(...).Scan(...)` 聚合。如果不行，**保留 Step 4 版本**（GORM 标准聚合 API 是合理使用）。

如果一定要用 generated：使用 `gorm.G[models.StorageObject](r.db).Where(...).Where(...).Count(ctx, "*")` 拿到 count，再单独 Sum（但 gorm gen 的 Sum 可能不直接暴露）。

**实际建议**：保留 Step 4 版本（GORM Model().Select().Scan 是标准聚合 API，参数化绑定，不是 raw SQL 字符串拼接）。

- [ ] **Step 6: 添加 GroupByVendorCountAndSumSizeByIDs**

```go

// GroupByVendorCountAndSumSizeByIDs groups active objects matching the IDs by
// vendor, returning count and total size per vendor. Single-table aggregation
// on storage_objects.
func (r *ObjectRepo) GroupByVendorCountAndSumSizeByIDs(ctx context.Context, ids []int64) ([]models.ProviderStatRow, error) {
	var result []models.ProviderStatRow
	if len(ids) == 0 {
		return result, nil
	}
	err := r.db.WithContext(ctx).
		Model(&models.StorageObject{}).
		Where("id IN ?", ids).
		Where("deleted_at IS NULL").
		Select("vendor, COUNT(*) AS object_count, COALESCE(SUM(size), 0) AS total_bytes").
		Group("vendor").
		Scan(&result).Error
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "group by vendor and sum size")
	}
	return result, nil
}

// GroupByBucketCountAndSumSizeByIDs groups active objects matching the IDs by
// bucket, returning count and total size per bucket. Single-table aggregation
// on storage_objects.
func (r *ObjectRepo) GroupByBucketCountAndSumSizeByIDs(ctx context.Context, ids []int64) ([]models.BucketObjectStatRow, error) {
	var result []models.BucketObjectStatRow
	if len(ids) == 0 {
		return result, nil
	}
	err := r.db.WithContext(ctx).
		Model(&models.StorageObject{}).
		Where("id IN ?", ids).
		Where("deleted_at IS NULL").
		Select("bucket, COUNT(*) AS object_count, COALESCE(SUM(size), 0) AS total_bytes").
		Group("bucket").
		Scan(&result).Error
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "group by bucket and sum size")
	}
	return result, nil
}
```

- [ ] **Step 7: 验证 store 包编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/store/...`
Expected: 编译通过

- [ ] **Step 8: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/repository/object.go
git commit -m "refactor(repository): object_repo → file, remove GetStats; add 5 single-table helpers"
```

---

## Task 6: 改造 quota_repo.go → quota.go（乐观锁）

**Files:**
- Rename: `internal/store/repository/quota_repo.go` → `internal/store/repository/quota.go`
- Modify: 上述文件内容

- [ ] **Step 1: git mv 重命名**

```bash
cd /Users/moss/code/base/storage-service
git mv internal/store/repository/quota_repo.go internal/store/repository/quota.go
```

- [ ] **Step 2: 改写 IncrementUsed 为乐观锁**

替换 `internal/store/repository/quota.go` 的 `IncrementUsed` 方法为：

```go
// IncrementUsed atomically increases the used bytes for an owner.
// Returns ErrQuotaExceeded if the increment would exceed total quota.
// Returns ErrQuotaConcurrentConflict if the quota row changed between read
// and update (caller should retry via service-layer withQuotaRetry).
func (r *QuotaRepo) IncrementUsed(ctx context.Context, ownerType int32, ownerID, bytes int64) error {
	quota, err := r.GetByOwner(ctx, ownerType, ownerID)
	if err != nil {
		return err
	}

	newUsed := quota.UsedBytes + bytes
	if newUsed > quota.TotalBytes {
		return xcodes.ErrQuotaExceeded.New()
	}

	rowsAffected, err := gorm.G[models.Quota](r.db).
		Where(generated.Quota.ID.Eq(quota.ID)).
		Where(generated.Quota.UsedBytes.Eq(quota.UsedBytes)). // CAS guard
		Set(generated.Quota.UsedBytes.Set(newUsed)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "increment used")
	}
	if rowsAffected == 0 {
		return xcodes.ErrQuotaConcurrentConflict.New()
	}
	return nil
}
```

- [ ] **Step 3: 改写 AddQuota 为乐观锁**

替换 `AddQuota` 方法为：

```go
// AddQuota atomically increments the owner's total quota by delta (may be
// negative for refund). Returns ErrQuotaInsufficientTotal if the refund would
// push total below zero. Returns ErrQuotaConcurrentConflict on concurrent
// modification (caller should retry).
func (r *QuotaRepo) AddQuota(ctx context.Context, ownerType int32, ownerID, delta int64) error {
	quota, err := r.GetByOwner(ctx, ownerType, ownerID)
	if err != nil {
		return err
	}

	newTotal := quota.TotalBytes + delta
	if newTotal < 0 {
		return xcodes.ErrQuotaInsufficientTotal.New()
	}

	rowsAffected, err := gorm.G[models.Quota](r.db).
		Where(generated.Quota.ID.Eq(quota.ID)).
		Where(generated.Quota.TotalBytes.Eq(quota.TotalBytes)). // CAS guard
		Set(generated.Quota.TotalBytes.Set(newTotal)).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrapf(err, "add quota")
	}
	if rowsAffected == 0 {
		return xcodes.ErrQuotaConcurrentConflict.New()
	}
	return nil
}
```

- [ ] **Step 4: 改 CreateIfNotExist 的 ON CONFLICT 列名**

在 `CreateIfNotExist` 方法中：

```go
// 旧
result := r.db.WithContext(ctx).
    Clauses(clause.OnConflict{
        Columns:   []clause.Column{{Name: "owner_type"}, {Name: "owner_id"}},
        DoNothing: true,
    }).
    Create(q)

// 新
result := r.db.WithContext(ctx).
    Clauses(clause.OnConflict{
        Columns: []clause.Column{
            generated.Quota.OwnerType.Column(),
            generated.Quota.OwnerID.Column(),
        },
        DoNothing: true,
    }).
    Create(q)
```

- [ ] **Step 5: 删除文件顶部不再需要的 imports**

如果 `clause.Expr` 不再被使用，从 import 中删除。检查 `gorm.io/gorm/clause` 是否还需要（用于 OnConflict，应该保留）。

Run: `cd /Users/moss/code/base/storage-service && goimports -w internal/store/repository/quota.go`

- [ ] **Step 6: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/store/...`
Expected: 编译通过

- [ ] **Step 7: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/repository/quota.go
git commit -m "refactor(repository): quota_repo → quota, replace raw SQL with optimistic lock"
```

---

## Task 7: 删除 stats_queries.go 和 table_name.go

**Files:**
- Delete: `internal/store/repository/stats_queries.go`
- Delete: `internal/store/repository/table_name.go`

- [ ] **Step 1: 删除文件**

```bash
cd /Users/moss/code/base/storage-service
git rm internal/store/repository/stats_queries.go internal/store/repository/table_name.go
```

- [ ] **Step 2: 验证 store 包编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/store/...`
Expected: 编译通过（stats_queries.go 的方法已在 Task 5/6 中重新归属，table_name.go 的 resolveTableName 已无人引用）

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/repository/
git commit -m "refactor(repository): remove stats_queries.go and table_name.go (cross-table SQL moved to service)"
```

---

## Task 8: 添加 service/types.go

**Files:**
- Create: `storage-service/internal/service/types.go`

- [ ] **Step 1: 创建文件**

```go
package service

// Cross-table aggregation rows (constructed in service layer by composing
// single-table repo results — cannot live in models/ because they don't
// correspond to a single model's table).

// OwnerStatRow holds per-owner-type aggregate statistics (cross-table:
// file.owner_type + object.size).
type OwnerStatRow struct {
	OwnerType  int32
	FileCount  int64
	TotalBytes int64
}

// BucketFileStatRow holds per-bucket file count statistics (cross-table:
// file count + object.bucket).
type BucketFileStatRow struct {
	Bucket    string
	FileCount int64
}

// StatsFilter defines optional filters for stats queries (used by service
// layer when composing getStorageStats).
type StatsFilter struct {
	OwnerType int32 // 0 = all
	OwnerID   int64 // 0 = all
}

// GlobalStats holds aggregate storage statistics returned to admin callers.
type GlobalStats struct {
	TotalObjects  int64
	PhysicalBytes int64
	TotalFiles    int64
	LogicalBytes  int64
	OwnerStats    []OwnerStat
	ProviderStats []ProviderStat
	BucketStats   []BucketStat
}

// OwnerStat is the wire-format per-owner-type aggregate.
type OwnerStat struct {
	OwnerType  int32
	FileCount  int64
	TotalBytes int64
}

// ProviderStat is the wire-format per-provider aggregate.
type ProviderStat struct {
	Vendor      int32
	ObjectCount int64
	TotalBytes  int64
}

// BucketStat is the wire-format per-bucket aggregate.
type BucketStat struct {
	Bucket      string
	ObjectCount int64
	TotalBytes  int64
	FileCount   int64
}
```

- [ ] **Step 2: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/service/...`
Expected: 编译通过

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/types.go
git commit -m "feat(service): add types.go for cross-table stats rows and GlobalStats"
```

---

## Task 9: 添加 service/quota.go::withQuotaRetry helper

**Files:**
- Modify: `storage-service/internal/service/quota.go`

- [ ] **Step 1: 查看现有 quota.go 结构**

Run: `cd /Users/moss/code/base/storage-service && head -30 internal/service/quota.go`
Expected: 看到 package + imports + ensureQuota 等函数

- [ ] **Step 2: 在 quota.go 末尾追加 withQuotaRetry**

```go
const quotaMaxRetries = 3

// withQuotaRetry retries QuotaRepo operations on ErrQuotaConcurrentConflict.
// QuotaRepo uses optimistic locking (read-then-CAS-update); under high
// concurrency the CAS guard may fail, requiring a fresh read and retry.
func (s *StorageService) withQuotaRetry(ctx context.Context, op func() error) error {
	var err error
	for attempt := 0; attempt < quotaMaxRetries; attempt++ {
		err = op()
		if err == nil {
			return nil
		}
		if !errors.Is(err, xcodes.ErrQuotaConcurrentConflict) {
			return err
		}
	}
	return xcodes.ErrQuotaConcurrentConflict.New("max retries exceeded")
}
```

- [ ] **Step 3: 检查 imports**

确保 `errors` 和 `storage-service/pkg/xcodes` 已 import。如果未 import，用 `goimports -w` 自动加：

Run: `cd /Users/moss/code/base/storage-service && goimports -w internal/service/quota.go`

- [ ] **Step 4: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/service/...`
Expected: 编译通过

- [ ] **Step 5: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/quota.go
git commit -m "feat(service): add withQuotaRetry helper for optimistic-lock retry"
```

---

## Task 10: 添加 service/stats.go（getStorageStats 组合）

**Files:**
- Create: `storage-service/internal/service/stats.go`

- [ ] **Step 1: 创建文件**

```go
package service

import (
	"context"
	"fmt"

	"storage-service/internal/store/generated"
	"storage-service/internal/store/models"
	"storage-service/pkg/xcodes"
)

// getStorageStats computes aggregate storage statistics by composing
// single-table repository methods. Replaces the old ObjectRepo.GetStats.
//
// Composition:
//   - TotalFiles: FileQuery.GetFileCount (single-table generated query)
//   - LogicalBytes: QuotaQuery.GetUsedBytes / GetTotalUsedBytes (single-table)
//   - TotalObjects + PhysicalBytes: ObjectRepo.CountActiveAndSumSizeByIDs
//     (when owner-filtered) — composed with FileRepo.FindObjectIDsByOwner
//   - OwnerStats: FileRepo.FindOwnerObjectIDPairs + ObjectRepo.BatchGetByIDs,
//     aggregated in memory by owner_type
//   - ProviderStats: ObjectRepo.GroupByVendorCountAndSumSizeByIDs
//   - BucketStats: ObjectRepo.GroupByBucketCountAndSumSizeByIDs + per-bucket
//     file count composed from FileRepo + ObjectRepo data
func (s *StorageService) getStorageStats(ctx context.Context, ownerType int32, ownerID int64) (*GlobalStats, error) {
	stats := &GlobalStats{}

	// 1. File count (single-table via generated).
	fileCountRow, err := generated.Query[models.File](s.db).GetFileCount(ctx, ownerType, ownerID)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "get file count")
	}
	stats.TotalFiles = fileCountRow.Count

	// 2. Used bytes (single-table via generated).
	if ownerType > 0 && ownerID > 0 {
		used, err := generated.Query[models.Quota](s.db).GetUsedBytes(ctx, ownerType, ownerID)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "get used bytes")
		}
		stats.LogicalBytes = used.UsedBytes
	} else if ownerType <= 0 {
		totalUsed, err := generated.Query[models.Quota](s.db).GetTotalUsedBytes(ctx)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "get total used bytes")
		}
		stats.LogicalBytes = totalUsed.UsedBytes
	}

	// 3. Object-side stats: physical/provider/bucket — all keyed by the set of
	//    object_ids reachable from the (owner-filtered) file table.
	var objectIDs []int64
	if ownerType > 0 && ownerID > 0 {
		ids, err := s.fileRepo.FindObjectIDsByOwner(ctx, ownerType, ownerID)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "find object ids by owner")
		}
		objectIDs = ids
	}

	// Physical stats.
	if ownerType > 0 && ownerID > 0 {
		physical, err := s.objectRepo.CountActiveAndSumSizeByIDs(ctx, objectIDs)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrapf(err, "physical stats")
		}
		stats.TotalObjects = physical.TotalObjects
		stats.PhysicalBytes = physical.PhysicalBytes
	}

	// Provider stats.
	providerRows, err := s.objectRepo.GroupByVendorCountAndSumSizeByIDs(ctx, objectIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "provider stats")
	}
	stats.ProviderStats = make([]ProviderStat, len(providerRows))
	for i, p := range providerRows {
		stats.ProviderStats[i] = ProviderStat{
			Vendor:      p.Vendor,
			ObjectCount: p.ObjectCount,
			TotalBytes:  p.TotalBytes,
		}
	}

	// Bucket object stats (also used as base for bucket file stats).
	bucketObjRows, err := s.objectRepo.GroupByBucketCountAndSumSizeByIDs(ctx, objectIDs)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "bucket object stats")
	}

	// Bucket file stats: file count per bucket (cross-table in memory).
	bucketFileCount, err := s.computeBucketFileCounts(ctx, ownerType, ownerID, len(bucketObjRows))
	if err != nil {
		return nil, err
	}

	stats.BucketStats = make([]BucketStat, len(bucketObjRows))
	for i, b := range bucketObjRows {
		stats.BucketStats[i] = BucketStat{
			Bucket:      b.Bucket,
			ObjectCount: b.ObjectCount,
			TotalBytes:  b.TotalBytes,
			FileCount:   bucketFileCount[b.Bucket],
		}
	}

	// Owner stats (cross-table in memory, only when no owner filter).
	if ownerType <= 0 {
		ownerRows, err := s.computeOwnerStats(ctx)
		if err != nil {
			return nil, err
		}
		stats.OwnerStats = ownerRows
	}

	return stats, nil
}

// computeBucketFileCounts returns a map of bucket -> file count for the given
// owner filter. Composed in memory from FileRepo + ObjectRepo single-table
// queries (no cross-table SQL).
func (s *StorageService) computeBucketFileCounts(ctx context.Context, ownerType int32, ownerID int64, expectedBuckets int) (map[string]int64, error) {
	pairs := make([]models.OwnerObjectIDPair, 0)
	if ownerType > 0 && ownerID > 0 {
		// Filter by owner: read (owner_type, object_id) pairs for this owner.
		ids, err := s.fileRepo.FindObjectIDsByOwner(ctx, ownerType, ownerID)
		if err != nil {
			return nil, fmt.Errorf("compute bucket file counts: %w", err)
		}
		for _, id := range ids {
			pairs = append(pairs, models.OwnerObjectIDPair{OwnerType: ownerType, ObjectID: id})
		}
	} else {
		all, err := s.fileRepo.FindOwnerObjectIDPairs(ctx)
		if err != nil {
			return nil, fmt.Errorf("compute bucket file counts: %w", err)
		}
		pairs = all
	}

	if len(pairs) == 0 {
		return make(map[string]int64), nil
	}

	// Unique object IDs for batch fetch.
	objIDSet := make(map[int64]struct{}, len(pairs))
	for _, p := range pairs {
		objIDSet[p.ObjectID] = struct{}{}
	}
	uniqueIDs := make([]int64, 0, len(objIDSet))
	for id := range objIDSet {
		uniqueIDs = append(uniqueIDs, id)
	}

	objects, err := s.objectRepo.BatchGetByIDs(ctx, uniqueIDs)
	if err != nil {
		return nil, fmt.Errorf("compute bucket file counts: batch get objects: %w", err)
	}

	// Count files per bucket (file count = occurrences of object_id in pairs).
	result := make(map[string]int64, expectedBuckets)
	for _, p := range pairs {
		if obj, ok := objects[p.ObjectID]; ok {
			result[obj.Bucket]++
		}
	}
	return result, nil
}

// computeOwnerStats returns per-owner-type aggregate (file count + total bytes).
// Composed in memory from FileRepo.FindOwnerObjectIDPairs + ObjectRepo size
// lookups (no cross-table SQL).
func (s *StorageService) computeOwnerStats(ctx context.Context) ([]OwnerStat, error) {
	pairs, err := s.fileRepo.FindOwnerObjectIDPairs(ctx)
	if err != nil {
		return nil, fmt.Errorf("compute owner stats: %w", err)
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	// Unique object IDs for batch fetch.
	objIDSet := make(map[int64]struct{}, len(pairs))
	for _, p := range pairs {
		objIDSet[p.ObjectID] = struct{}{}
	}
	uniqueIDs := make([]int64, 0, len(objIDSet))
	for id := range objIDSet {
		uniqueIDs = append(uniqueIDs, id)
	}

	objects, err := s.objectRepo.BatchGetByIDs(ctx, uniqueIDs)
	if err != nil {
		return nil, fmt.Errorf("compute owner stats: batch get objects: %w", err)
	}

	// Aggregate by owner_type.
	type agg struct {
		FileCount  int64
		TotalBytes int64
	}
	byOwner := make(map[int32]*agg)
	for _, p := range pairs {
		a, ok := byOwner[p.OwnerType]
		if !ok {
			a = &agg{}
			byOwner[p.OwnerType] = a
		}
		a.FileCount++
		if obj, ok := objects[p.ObjectID]; ok {
			a.TotalBytes += obj.Size
		}
	}

	result := make([]OwnerStat, 0, len(byOwner))
	for ownerType, a := range byOwner {
		result = append(result, OwnerStat{
			OwnerType:  ownerType,
			FileCount:  a.FileCount,
			TotalBytes: a.TotalBytes,
		})
	}
	return result, nil
}
```

- [ ] **Step 2: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/service/...`
Expected: 编译通过（admin.go 仍引用旧的 s.objectRepo.GetStats —— 下个 Task 修复，暂时报错可接受）

- [ ] **Step 3: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/stats.go
git commit -m "feat(service): add stats.go with getStorageStats composing single-table repo methods"
```

---

## Task 11: 修改 service/file.go::listFiles

**Files:**
- Modify: `storage-service/internal/service/file.go`

- [ ] **Step 1: 查看当前 listFiles 实现**

Run: `cd /Users/moss/code/base/storage-service && sed -n '50,100p' internal/service/file.go`
Expected: 看到 listFiles 函数和 fileRepo.ListByOwner 调用点

- [ ] **Step 2: 在 listFiles 中加入 ContentTypePrefix 预查**

找到类似下面这段：

```go
filter := repository.ListFilesFilter{
    PathPrefix:        req.GetPathPrefix(),
    Extension:         req.GetExtension(),
    ContentTypePrefix: req.GetContentTypePrefix(),
    OrderBy:           req.GetOrderBy(),
    Descending:        req.GetDescending(),
    Pagination:        dbx.Pagination{...},
}

files, total, err := s.fileRepo.ListByOwner(ctx, ownerID, ownerType, filter)
```

改为：

```go
filter := repository.ListFilesFilter{
    PathPrefix:        req.GetPathPrefix(),
    Extension:         req.GetExtension(),
    ContentTypePrefix: req.GetContentTypePrefix(),
    OrderBy:           req.GetOrderBy(),
    Descending:        req.GetDescending(),
    Pagination:        dbx.Pagination{...},
}

// Cross-table filter resolved at service layer: look up object IDs first.
var objectIDs []int64
if req.GetContentTypePrefix() != "" {
    ids, err := s.objectRepo.FindIDsByContentTypePrefix(ctx, req.GetContentTypePrefix())
    if err != nil {
        return nil, fmt.Errorf("find object ids by content type: %w", err)
    }
    if len(ids) == 0 {
        return &storagev1.ListFilesResponse{}, nil
    }
    objectIDs = ids
}

files, total, err := s.fileRepo.ListByOwner(ctx, ownerID, ownerType, filter, objectIDs)
```

注意：保持原有 `return nil, total` 等返回结构不变，只修改 ListByOwner 调用。

- [ ] **Step 3: 验证 service 编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/service/...`
Expected: 编译通过（admin.go 仍可能报错，下个 Task 修）

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/file.go
git commit -m "refactor(service): listFiles resolves ContentTypePrefix to objectIDs before calling FileRepo"
```

---

## Task 12: 修改 service/admin.go::adminListFiles

**Files:**
- Modify: `storage-service/internal/service/admin.go`

- [ ] **Step 1: 查看当前 adminListFiles**

Run: `cd /Users/moss/code/base/storage-service && grep -n "adminListFiles\|s.fileRepo.ListAll" internal/service/admin.go`
Expected: 找到函数定义和 ListAll 调用行号

- [ ] **Step 2: 改造 adminListFiles 加入 object ID 预查**

找到类似下面这段：

```go
filter := repository.AdminListFilesFilter{
    OwnerType:         int32(req.GetOwnerType()),
    OwnerID:           req.GetOwnerId(),
    PathPrefix:        req.GetPathPrefix(),
    Extension:         req.GetExtension(),
    ContentTypePrefix: req.GetContentTypePrefix(),
    Vendor:            vendor,
    Bucket:            req.GetBucket(),
    OrderBy:           req.GetOrderBy(),
    Descending:        req.GetDescending(),
    Pagination:        dbx.Pagination{...},
}

files, total, err := s.fileRepo.ListAll(ctx, filter)
```

改为：

```go
filter := repository.AdminListFilesFilter{
    OwnerType:         int32(req.GetOwnerType()),
    OwnerID:           req.GetOwnerId(),
    PathPrefix:        req.GetPathPrefix(),
    Extension:         req.GetExtension(),
    ContentTypePrefix: req.GetContentTypePrefix(),
    Vendor:            vendor,
    Bucket:            req.GetBucket(),
    OrderBy:           req.GetOrderBy(),
    Descending:        req.GetDescending(),
    Pagination:        dbx.Pagination{...},
}

// Cross-table filters resolved at service layer.
var objectIDs []int64
needObjectJoin := req.GetContentTypePrefix() != "" || vendor != 0 || req.GetBucket() != ""
if needObjectJoin {
    ids, err := s.objectRepo.FindIDsByFilter(ctx, req.GetContentTypePrefix(), vendor, req.GetBucket())
    if err != nil {
        return nil, xcodes.ErrInternal.Wrap(err)
    }
    if len(ids) == 0 {
        return &storagev1.AdminListFilesResponse{}, nil
    }
    objectIDs = ids
}

files, total, err := s.fileRepo.ListAll(ctx, filter, objectIDs)
```

- [ ] **Step 3: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./internal/service/...`
Expected: adminListFiles 部分通过（adminGetStats 仍可能报错，下个 Task 修）

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/admin.go
git commit -m "refactor(service): adminListFiles resolves object-side filters at service layer"
```

---

## Task 13: 修改 service/admin.go::adminGetStats

**Files:**
- Modify: `storage-service/internal/service/admin.go`

- [ ] **Step 1: 替换 GetStats 调用**

找到 `adminGetStats` 函数（约 184 行附近）：

```go
// 旧
stats, err := s.objectRepo.GetStats(ctx, repository.StatsFilter{
    OwnerType: int32(req.GetOwnerType()),
    OwnerID:   req.GetOwnerId(),
})
```

改为：

```go
// 新
stats, err := s.getStorageStats(ctx, int32(req.GetOwnerType()), req.GetOwnerId())
```

注意：`StatsFilter` 类型现在在 `service` 包内（不在 `repository` 包），所以可以直接传 `int32, int64` 参数。如果 `getStorageStats` 签名定义为接收 `(*StatsFilter)`，则改 `s.getStorageStats(ctx, &StatsFilter{OwnerType: ..., OwnerID: ...})`。本计划使用前者（更简洁）。

- [ ] **Step 2: 删除 admin.go 顶部不再使用的 repository StatsFilter 引用（如有）**

如果 admin.go 顶部 `import` 中有 `"storage-service/internal/store/repository"` 但只在 StatsFilter 用，并且现在没有其他 repository 引用，需要删除 import。检查方法：

Run: `cd /Users/moss/code/base/storage-service && grep -n "repository\." internal/service/admin.go`
Expected: 看是否还有其他 repository.Xxx 引用，如果有保留 import，没有则 goimports 自动删

Run: `cd /Users/moss/code/base/storage-service && goimports -w internal/service/admin.go`

- [ ] **Step 3: 验证整体编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 编译通过（整个项目首次完整编译通过）

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/admin.go
git commit -m "refactor(service): adminGetStats uses service-layer getStorageStats composition"
```

---

## Task 14: 包装 QuotaRepo 调用使用 withQuotaRetry

**Files:**
- Modify: 多个 service 文件（包含 `quotaRepo.IncrementUsed`、`DecrementUsed`、`AddQuota` 调用的地方）

- [ ] **Step 1: 找出所有 QuotaRepo 乐观锁方法调用点**

Run: `cd /Users/moss/code/base/storage-service && grep -rn "quotaRepo\.\(IncrementUsed\|DecrementUsed\|AddQuota\)\|s\.quotaRepo\.\(IncrementUsed\|DecrementUsed\|AddQuota\)" internal/`
Expected: 列出所有调用点（应在 service/file.go, service/admin.go, service/quota.go 等）

- [ ] **Step 2: 对每个调用点用 withQuotaRetry 包装**

例如，原代码：

```go
if err := s.quotaRepo.IncrementUsed(ctx, ownerType, ownerID, size); err != nil {
    return err
}
```

改为：

```go
if err := s.withQuotaRetry(ctx, func() error {
    return s.quotaRepo.IncrementUsed(ctx, ownerType, ownerID, size)
}); err != nil {
    return err
}
```

对每个调用点重复此改造。如果调用点很多（>5 个），考虑加一个 service 方法 `s.incrQuotaUsed(...)` 等做内嵌重试，然后 service 调 service 方法。

- [ ] **Step 3: 验证编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 编译通过

- [ ] **Step 4: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/
git commit -m "refactor(service): wrap QuotaRepo optimistic-lock calls with withQuotaRetry"
```

---

## Task 15: 最终验证（gofmt + lint + test + migrate）

**Files:** 无修改，仅验证

- [ ] **Step 1: gofmt + goimports 全部改动文件**

```bash
cd /Users/moss/code/base/storage-service
gofmt -w internal/store/ internal/service/ pkg/xcodes/
goimports -w internal/store/ internal/service/ pkg/xcodes/
```

- [ ] **Step 2: golangci-lint**

Run: `cd /Users/moss/code/base/storage-service && golangci-lint run ./internal/store/... ./internal/service/... ./pkg/xcodes/...`
Expected: 无错误（warning 可接受，error 必须修）

- [ ] **Step 3: 单元测试**

Run: `cd /Users/moss/code/base/storage-service && go test ./internal/store/... ./internal/service/...`
Expected: 现有测试全部 PASS（upload_session_test, service_test 等）

- [ ] **Step 4: 整体编译**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 无错误

- [ ] **Step 5: 跑 AutoMigrate 验证 model 没破坏**

Run: `cd /Users/moss/code/base/storage-service && make migrate`
Expected: 命令成功（连接数据库做 AutoMigrate，建表/索引正常）

注意：需要数据库连接（PostgreSQL），如本地无 DB，可跳过此步或用 testcontainer。

- [ ] **Step 6: 跑 gorm gen 重新生成（再确认一次）**

Run: `cd /Users/moss/code/base/storage-service && make generate`
Expected: 命令成功，`internal/store/generated/` 文件被刷新

如果 generated 代码有 diff（说明 model 改了），跑 `git diff internal/store/generated/` 看变化，确认无破坏。

- [ ] **Step 7: 提交 generated 同步（如有 diff）**

```bash
cd /Users/moss/code/base/storage-service
git add internal/store/generated/
git diff --cached --stat
# 如有 diff
git commit -m "chore(generated): sync gorm gen output after models refactor"
```

- [ ] **Step 8: 最终 commit（如果还有未提交的格式化等小修改）**

```bash
cd /Users/moss/code/base/storage-service
git status
# 如有未提交修改
git add -A
git commit -m "style: gofmt/goimports cleanup"
```

---

## Self-Review

**Spec coverage check**:
- ✅ models/X.go ↔ repository/X.go 一一对应（Task 3-7）
- ✅ repository 零 raw SQL（Task 4-7）
- ✅ 跨表逻辑推 service 层组合（Task 10-13）
- ✅ 删除 stats_queries.go（Task 7）
- ✅ 删除 table_name.go（Task 7）
- ✅ 删除 models/query.go（Task 2）
- ✅ Row 类型按 model 归位（Task 2）
- ✅ gen 注解按 model 拆（Task 2）
- ✅ ObjectRepo 新增 5 个单表方法（Task 5）
- ✅ FileRepo 新增 FindObjectIDsByOwner / FindOwnerObjectIDPairs（Task 4）
- ✅ FileRepo API 改造接受 objectIDs（Task 4）
- ✅ QuotaRepo IncrementUsed/AddQuota 改乐观锁（Task 6）
- ✅ ON CONFLICT 列名用 generated.XXX.Column()（Task 5, 6）
- ✅ service/types.go（Task 8）
- ✅ service/stats.go getStorageStats（Task 10）
- ✅ service/quota.go withQuotaRetry（Task 9）
- ✅ listFiles 改造（Task 11）
- ✅ adminListFiles 改造（Task 12）
- ✅ adminGetStats 改造（Task 13）
- ✅ ErrQuotaConcurrentConflict（Task 1）
- ✅ withQuotaRetry 包装调用（Task 14）
- ✅ 验证清单（Task 15）

**Placeholder scan**: 无 TODO/TBD/省略号占位，所有代码完整。

**Type consistency check**:
- `models.FileQuery`, `models.ObjectQuery`, `models.QuotaQuery` 接口在 Task 2 定义，后续 Task 引用一致
- `models.OwnerObjectIDPair` 在 Task 2 (Step 1) 定义，Task 4 (Step 4-5) 和 Task 10 引用
- `models.PhysicalStatsRow` 在 Task 2 (Step 2) 定义，Task 5 (Step 4) 返回，Task 10 使用
- `models.ProviderStatRow`, `models.BucketObjectStatRow` 同上
- `service.OwnerStatRow`, `service.BucketFileStatRow`, `service.GlobalStats`, `service.OwnerStat`, `service.ProviderStat`, `service.BucketStat`, `service.StatsFilter` 在 Task 8 定义，Task 10/13 引用
- `withQuotaRetry` 在 Task 9 定义，Task 14 引用
- `ErrQuotaConcurrentConflict` 在 Task 1 定义，Task 6/9 引用
- `ObjectRepo.FindIDsByContentTypePrefix` / `FindIDsByFilter` 在 Task 5 定义，Task 11/12 引用
- `ObjectRepo.CountActiveAndSumSizeByIDs` / `GroupByVendorCountAndSumSizeByIDs` / `GroupByBucketCountAndSumSizeByIDs` 在 Task 5 定义，Task 10 引用
- `FileRepo.FindObjectIDsByOwner` / `FindOwnerObjectIDPairs` 在 Task 4 定义，Task 10 引用
- `FileRepo.ListByOwner` / `ListAll` 新签名（加 objectIDs []int64）在 Task 4 定义，Task 11/12 引用

**Scope check**: 单一实施计划可覆盖，范围适中。15 个 task，每个独立可验证。

**Ambiguity check**:
- `FindOwnerObjectIDPairs` 的实现版本（Step 4 raw Table+Select vs Step 5 generated field）—— Task 4 内 Step 5 明确选择更纯净版本
- `CountActiveAndSumSizeByIDs` 等的 Model().Select() 实现 —— Task 5 Step 5 说明这是 GORM 标准聚合 API，参数化绑定，合理
