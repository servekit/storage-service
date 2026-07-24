# store 目录重构：models 与 repository 文件一一对应

**日期**：2026-06-17
**范围**：`storage-service/internal/store/`
**关联**：连带修改 `storage-service/internal/service/`

## 背景

当前 `internal/store/` 下的 `models/` 与 `repository/` 文件命名不对应，且 repository 文件中混入了大量跨表操作（`stats_queries.go`、`object_repo.go::GetStats`、`file_repo.go::ListByOwner/ListAll` 内部跨表查询），违反"对应文件只操作对应 model"的原则。

## 目标

1. `models/X.go` 与 `repository/X.go` 严格一一对应
2. repository 层每个文件**只**操作自己 model 对应的单张表
3. repository 层**零 raw SQL**：跨表逻辑推到 service 层，单表跨列算术条件改用乐观锁模式
4. 跨表逻辑由 service 层通过组合各 repository 的单表方法实现（参考 `service.ensureQuota` 模式）

## 设计原则

- **repository 层**：单表 CRUD + 单表聚合（COUNT、SUM、GROUP BY 等不跨表）
- **service 层**：通过组合各 repository 的单表方法做跨表编排，**不写 raw SQL**
- **命名约定**：repository 文件去掉 `_repo` 后缀，与 models 同名

参考实现模式（`service/quota.go::ensureQuota`）：
```go
func (s *StorageService) ensureQuota(ctx context.Context, db *gorm.DB, ownerType int32, ownerID int64) (*models.Quota, error) {
    quotaRepo := repository.NewQuotaRepo(db)
    quota, err := quotaRepo.GetByOwner(ctx, ownerType, ownerID)
    if err == nil {
        return quota, nil
    }
    // ... 组合其他 repo 调用 ...
}
```

## 现状清单

### models/（7 个文件）

| 文件 | 内容 | 问题 |
|------|------|------|
| `audit_log.go` | AuditLog, JSONMap | OK |
| `file.go` | File, MapJSON | OK |
| `object.go` | StorageObject | OK |
| `quota.go` | Quota | OK |
| `upload_session.go` | UploadSession, Status 常量 | OK |
| `query.go` | 8 个 Row 类型 + 泛型 `Query[T]` gen 接口 | 杂糅多个 model |
| `register.go` | AllModels(), gorm gen 配置 | OK（共享元信息） |

### repository/（9 个文件，含 1 测试）

| 文件 | 内容 | 问题 |
|------|------|------|
| `audit_log_repo.go` | AuditLogRepo | 命名后缀 |
| `file_repo.go` | FileRepo + `ListByOwner/ListAll` 内部跨表查 StorageObject | 跨表污染 |
| `object_repo.go` | ObjectRepo + `GetStats`（跨 4 model） + 跨表 stats 类型 | 严重跨表 |
| `quota_repo.go` | QuotaRepo | 命名后缀 |
| `upload_session_repo.go` + 测试 | UploadSessionRepo | 命名后缀 |
| `stats_queries.go` | ObjectRepo/FileRepo 的 5 个跨表 stats 方法 + helper | 完全跨表，无对应 model |
| `constants.go` | MaxBatchSize | OK（共享） |
| `table_name.go` | resolveTableName（仅 raw SQL 用，本方案不再需要） | 可删除 |

## 重构方案

### 文件对应关系（最终）

```
models/                              repository/
├── audit_log.go          ←──→      ├── audit_log.go
├── file.go               ←──→      ├── file.go
├── object.go             ←──→      ├── object.go
├── quota.go              ←──→      ├── quota.go
├── upload_session.go     ←──→      ├── upload_session.go
└── register.go                      ├── upload_session_test.go
                                     └── constants.go
```

### 操作清单

#### 1. 文件重命名

| 原文件 | 新文件 |
|--------|--------|
| `repository/audit_log_repo.go` | `repository/audit_log.go` |
| `repository/file_repo.go` | `repository/file.go` |
| `repository/object_repo.go` | `repository/object.go` |
| `repository/quota_repo.go` | `repository/quota.go` |
| `repository/upload_session_repo.go` | `repository/upload_session.go` |
| `repository/upload_session_repo_test.go` | `repository/upload_session_test.go` |

#### 2. 删除文件

- `repository/stats_queries.go`（跨表 SQL 拆解为单表方法，逻辑移到 service 层组合）
- `repository/table_name.go`（`resolveTableName` 仅被 raw SQL 使用，本方案不再需要）
- `models/query.go`（Row 类型和 gen 注解按 model 拆分到各自文件）

#### 3. models/query.go 拆分

**Row 类型按"主要查询表"归位**（拆解后的单表聚合结果属于该表）：

| Row 类型 | 主表 | 归属文件 |
|---------|------|---------|
| `FileCountRow` | file | `models/file.go` |
| `ObjectRefCountRow` | file | `models/file.go` |
| `UsedBytesRow` | quota | `models/quota.go` |
| `PhysicalStatsRow` | object（单表 COUNT/SUM） | `models/object.go` |
| `ProviderStatRow` | object（单表 GROUP BY vendor） | `models/object.go` |
| `BucketObjectStatRow` | object（单表 GROUP BY bucket） | `models/object.go` |
| `OwnerStatRow` | 跨表（file.owner_type + object.size） | `service/types.go` |
| `BucketFileStatRow` | 跨表（file count + object.bucket） | `service/types.go` |

**gen 注解接口按 model 拆**：

- `models/file.go` 新增 `FileQuery` 接口（含 `GetActiveByID`, `GetObjectRefCounts`, `GetFileCount`）
- `models/quota.go` 新增 `QuotaQuery` 接口（含 `GetActiveByID`, `GetUsedBytes`, `GetTotalUsedBytes`）
- `models/object.go` 新增 `ObjectQuery` 接口（如 generated 代码用到，含 `GetActiveByID`）

`GetActiveByID` 在每个 model 文件重复定义，换取文件清晰对应。

#### 4. repository/object.go 改造

**移除**：
- 方法：`GetStats`, `GetPhysicalStats`, `GetProviderStats`, `GetBucketObjectStats`
- 类型：`StatsFilter`, `GlobalStats`, `OwnerStat`, `ProviderStat`, `BucketStat`（迁到 `service/types.go`）

**保留**：所有单表方法（`FindByVendorBucketMD5`, `GetByID`, `BatchGetByIDs`, `CreateOrGet`, `IncrRefCount`, `DecrRefCount`, `DecrRefCountBy`, `SoftDelete`, `SoftDeleteZeroRefCount`, `FindPurgeable`, `HardDelete`, `FindByObjectKey`, `BatchFindObjectKeys`）

**改造：`CreateOrGet` 的 ON CONFLICT 列名改用 generated.Column()**（消除字符串硬编码）：

```go
// 旧
Clauses(clause.OnConflict{
    Columns:   []clause.Column{{Name: "vendor"}, {Name: "bucket"}, {Name: "md5"}},
    DoNothing: true,
}).

// 新
Clauses(clause.OnConflict{
    Columns: []clause.Column{
        generated.StorageObject.Vendor.Column(),
        generated.StorageObject.Bucket.Column(),
        generated.StorageObject.MD5.Column(),
    },
    DoNothing: true,
}).
```

**新增单表辅助方法（为 service 层组合 stats 和 file list 过滤服务）**：

```go
// FindIDsByContentTypePrefix returns IDs of active objects whose content type
// starts with prefix. Single-table query. Used by service layer for file list
// filtering.
func (r *ObjectRepo) FindIDsByContentTypePrefix(ctx context.Context, prefix string) ([]int64, error)

// FindIDsByFilter returns IDs of active objects matching the optional filters.
// Zero-value filters are ignored. Single-table query. Used by service layer
// for admin file list.
func (r *ObjectRepo) FindIDsByFilter(ctx context.Context, contentTypePrefix string, vendor int32, bucket string) ([]int64, error)

// CountActiveAndSumSizeByIDs returns the count and total size of active objects
// matching the given IDs. Single-table aggregation.
// Used by service layer to compute PhysicalStats (after fetching object IDs from FileRepo).
func (r *ObjectRepo) CountActiveAndSumSizeByIDs(ctx context.Context, ids []int64) (models.PhysicalStatsRow, error)

// GroupByVendorCountAndSumSizeByIDs groups active objects by vendor, returning
// count and total size per vendor for the given IDs. Single-table aggregation.
func (r *ObjectRepo) GroupByVendorCountAndSumSizeByIDs(ctx context.Context, ids []int64) ([]models.ProviderStatRow, error)

// GroupByBucketCountAndSumSizeByIDs groups active objects by bucket, returning
// count and total size per bucket for the given IDs. Single-table aggregation.
func (r *ObjectRepo) GroupByBucketCountAndSumSizeByIDs(ctx context.Context, ids []int64) ([]models.BucketObjectStatRow, error)
```

#### 5. repository/file.go 改造

**移除**：`ListByOwner` 和 `ListAll` 内部对 `models.StorageObject` 的查询逻辑

**签名变更**：

```go
// 旧
func (r *FileRepo) ListByOwner(ctx context.Context, ownerID int64, ownerType int32, filter ListFilesFilter) ([]models.File, int, error)

// 新
func (r *FileRepo) ListByOwner(ctx context.Context, ownerID int64, ownerType int32, filter ListFilesFilter, objectIDs []int64) ([]models.File, int, error)
```

`ListAll` 同理增加 `objectIDs []int64` 参数。

- 当 `objectIDs` 非 nil 且非空：加入 `WHERE object_id IN (...)` 过滤
- 当 `objectIDs` 是空切片 `[]int64{}`：表示过滤后无匹配，直接返回空结果（避免全表扫）
- 当 `objectIDs` 为 nil：不应用此过滤（保持原行为）

`ListFilesFilter` 和 `AdminListFilesFilter` 中的 `ContentTypePrefix`、`Vendor`、`Bucket` 字段保留（service 层用它们决定是否查 object IDs），但 file_repo 内部不再读取这些字段做跨表查询。

**新增单表辅助方法（为 service 层 stats 组合服务）**：

```go
// FindObjectIDsByOwner returns object_ids referenced by active files of the
// given owner. May contain duplicates (multiple files per object).
// Single-table query. Used by service layer to compute Object/File stats
// (PhysicalStats, ProviderStats, BucketObjectStats, BucketFileStats).
func (r *FileRepo) FindObjectIDsByOwner(ctx context.Context, ownerType int32, ownerID int64) ([]int64, error)

// FindOwnerObjectIDPairs returns (owner_type, object_id) pairs for all active
// files. Single-table query. Used by service layer to compute OwnerStats
// (file count per owner_type + sum of object sizes per owner_type).
// Note: returns full table scan; for large datasets consider batching.
func (r *FileRepo) FindOwnerObjectIDPairs(ctx context.Context) ([]models.OwnerObjectIDPair, error)
```

`OwnerObjectIDPair` 类型新增到 `models/file.go`：
```go
type OwnerObjectIDPair struct {
    OwnerType int32 `gorm:"column:owner_type"`
    ObjectID  int64 `gorm:"column:object_id"`
}
```

#### 6. repository/quota.go 改造

文件重命名 + 消除 2 处单表跨列算术 raw SQL（改用乐观锁模式）：

**IncrementUsed**（原 `Where("used_bytes + ? <= total_bytes", bytes)`）：

```go
// 旧：raw SQL 表达跨列算术条件
rowsAffected, err := gorm.G[models.Quota](r.db).
    Where(generated.Quota.OwnerType.Eq(ownerType)).
    Where(generated.Quota.OwnerID.Eq(ownerID)).
    Where(generated.Quota.DeletedAt.IsNull()).
    Where("used_bytes + ? <= total_bytes", bytes).  // ← raw SQL
    Set(generated.Quota.UsedBytes.Incr(bytes)).
    Update(ctx)

// 新：乐观锁（Get + CAS Update）
func (r *QuotaRepo) IncrementUsed(ctx context.Context, ownerType int32, ownerID, bytes int64) error {
    quota, err := r.GetByOwner(ctx, ownerType, ownerID)
    if err != nil { return err }

    newUsed := quota.UsedBytes + bytes
    if newUsed > quota.TotalBytes {
        return xcodes.ErrQuotaExceeded.New()
    }

    rows, err := gorm.G[models.Quota](r.db).
        Where(generated.Quota.ID.Eq(quota.ID)).
        Where(generated.Quota.UsedBytes.Eq(quota.UsedBytes)). // CAS：检测并发修改
        Set(generated.Quota.UsedBytes.Set(newUsed)).
        Update(ctx)
    if err != nil { return xcodes.ErrInternal.Wrapf(err, "increment used") }
    if rows == 0 {
        return xcodes.ErrQuotaConcurrentConflict.New()
    }
    return nil
}
```

**AddQuota**（原 `Where(clause.Expr{SQL: "total_bytes + ? >= 0", Vars: []any{delta}})`）：

```go
// 新：乐观锁
func (r *QuotaRepo) AddQuota(ctx context.Context, ownerType int32, ownerID, delta int64) error {
    quota, err := r.GetByOwner(ctx, ownerType, ownerID)
    if err != nil { return err }

    newTotal := quota.TotalBytes + delta
    if newTotal < 0 {
        return xcodes.ErrQuotaInsufficientTotal.New()
    }

    rows, err := gorm.G[models.Quota](r.db).
        Where(generated.Quota.ID.Eq(quota.ID)).
        Where(generated.Quota.TotalBytes.Eq(quota.TotalBytes)). // CAS
        Set(generated.Quota.TotalBytes.Set(newTotal)).
        Update(ctx)
    if err != nil { return xcodes.ErrInternal.Wrapf(err, "add quota") }
    if rows == 0 {
        return xcodes.ErrQuotaConcurrentConflict.New()
    }
    return nil
}
```

**DecrementUsed** 不动（`Where(generated.Quota.UsedBytes.Gte(bytes))` 是 generated 标准用法，非 raw SQL）。

**ON CONFLICT 列名改用 generated.Column()**（消除字符串硬编码）：

```go
// 旧
Clauses(clause.OnConflict{
    Columns:   []clause.Column{{Name: "owner_type"}, {Name: "owner_id"}},
    DoNothing: true,
}).

// 新
Clauses(clause.OnConflict{
    Columns: []clause.Column{
        generated.Quota.OwnerType.Column(),
        generated.Quota.OwnerID.Column(),
    },
    DoNothing: true,
}).
```

**新增错误码**（在 `pkg/xcodes/`）：
- `ErrQuotaConcurrentConflict` —— 乐观锁冲突，service 层捕获后重试

**service 层重试封装**（新增 `service/quota.go::withQuotaRetry`）：

```go
const quotaMaxRetries = 3

// withQuotaRetry retries QuotaRepo operations on ErrQuotaConcurrentConflict.
func (s *StorageService) withQuotaRetry(ctx context.Context, op func() error) error {
    for attempt := 0; attempt < quotaMaxRetries; attempt++ {
        err := op()
        if err == nil { return nil }
        if !errors.Is(err, xcodes.ErrQuotaConcurrentConflict) {
            return err
        }
    }
    return xcodes.ErrQuotaConcurrentConflict.New("max retries exceeded")
}
```

调用点（`service/file.go`, `service/admin.go` 等）包装 QuotaRepo 的 IncrementUsed / DecrementUsed / AddQuota 调用。

#### 7. repository/audit_log.go 改造

无功能性改动，仅文件重命名。

#### 8. repository/upload_session.go 改造

无功能性改动，仅文件重命名。测试文件跟随重命名。

#### 9. repository/constants.go 保留

`MaxBatchSize` 是共享常量，不对应任何单一 model。

### service 层改造

#### 新增 `service/types.go`

跨表聚合 Row 类型 + 编排结果类型：

```go
package service

// 跨表聚合结果（service 层组合各 repo 单表查询后构造）
type OwnerStatRow struct {
    OwnerType  int32
    FileCount  int64
    TotalBytes int64
}

type BucketFileStatRow struct {
    Bucket    string
    FileCount int64
}

// 业务编排结果
type StatsFilter struct {
    OwnerType int32 // 0 = all
    OwnerID   int64 // 0 = all
}

type GlobalStats struct {
    TotalObjects  int64
    PhysicalBytes int64
    TotalFiles    int64
    LogicalBytes  int64
    OwnerStats    []OwnerStat
    ProviderStats []ProviderStat
    BucketStats   []BucketStat
}

type OwnerStat struct {
    OwnerType  int32
    FileCount  int64
    TotalBytes int64
}

type ProviderStat struct {
    Vendor      int32
    ObjectCount int64
    TotalBytes  int64
}

type BucketStat struct {
    Bucket      string
    ObjectCount int64
    TotalBytes  int64
    FileCount   int64
}
```

#### 新增 `service/stats.go`

通过组合各 repository 的单表方法实现 stats 聚合（不写 raw SQL）：

```go
package service

// getStorageStats computes aggregate storage statistics by composing
// single-table repository methods. Replaces the old ObjectRepo.GetStats.
//
// Composition:
//   - physical stats: FileRepo.FindObjectIDsByOwner → ObjectRepo.CountActiveAndSumSizeByIDs
//   - provider stats: FileRepo.FindObjectIDsByOwner → ObjectRepo.GroupByVendorCountAndSumSizeByIDs
//   - bucket object stats: FileRepo.FindObjectIDsByOwner → ObjectRepo.GroupByBucketCountAndSumSizeByIDs
//   - bucket file stats: FileRepo.FindObjectIDsByOwner + ObjectRepo.BatchGetByIDs → 内存按 bucket 聚合
//   - owner stats: FileRepo.FindOwnerObjectIDPairs + ObjectRepo.BatchGetByIDs → 内存按 owner_type 聚合
//   - file count: existing FileRepo.CountByOwner (or FileQuery.GetFileCount via generated)
//   - used bytes: existing QuotaRepo / QuotaQuery via generated
func (s *StorageService) getStorageStats(ctx context.Context, ownerType int32, ownerID int64) (*GlobalStats, error) {
    stats := &GlobalStats{}

    // 1. Owner 范围内的 object_ids（去重前可能含重复）
    var objectIDs []int64
    if ownerType > 0 && ownerID > 0 {
        ids, err := s.fileRepo.FindObjectIDsByOwner(ctx, ownerType, ownerID)
        if err != nil { return nil, fmt.Errorf("find object ids by owner: %w", err) }
        objectIDs = ids
    }

    // 2. Physical stats (single-table aggregation on objects)
    if ownerType > 0 && ownerID > 0 {
        physical, err := s.objectRepo.CountActiveAndSumSizeByIDs(ctx, objectIDs)
        if err != nil { return nil, fmt.Errorf("count active objects: %w", err) }
        stats.TotalObjects = physical.TotalObjects
        stats.PhysicalBytes = physical.PhysicalBytes
    } else {
        // No owner filter → simple global counts (new ObjectRepo method or use existing query)
        // Implementation note: may need ObjectRepo.CountAllActiveAndSumSize() helper
    }

    // 3. File count
    fileCount, err := generated.Query[models.File](s.db).GetFileCount(ctx, ownerType, ownerID)
    if err != nil { return nil, fmt.Errorf("get file count: %w", err) }
    stats.TotalFiles = fileCount.Count

    // 4. Quota / used bytes
    // ... 调用 QuotaRepo / generated.Query[models.Quota] ...

    // 5. Owner stats (cross-table compose in memory)
    if ownerType <= 0 {
        pairs, err := s.fileRepo.FindOwnerObjectIDPairs(ctx)
        if err != nil { return nil, fmt.Errorf("find owner object pairs: %w", err) }
        objectSizeMap := s.batchGetObjectSizes(ctx, uniqueObjectIDs(pairs))
        stats.OwnerStats = composeOwnerStats(pairs, objectSizeMap)
    }

    // 6. Provider / bucket stats via single-table aggregation
    // ...

    // 7. Bucket file stats (cross-table compose in memory)
    // ...

    return stats, nil
}
```

具体实现细节在 writing-plans 阶段展开。关键约束：**所有 DB 访问通过 repository 方法，service 层只做调用编排和内存组合**。

#### 修改 `service/file.go::listFiles`

```go
func (s *StorageService) listFiles(ctx context.Context, req *storagev1.ListFilesRequest) (*storagev1.ListFilesResponse, error) {
    // ... 原有 owner 解析等逻辑 ...

    filter := repository.ListFilesFilter{
        PathPrefix:        req.GetPathPrefix(),
        Extension:         req.GetExtension(),
        ContentTypePrefix: req.GetContentTypePrefix(), // 保留字段，但 fileRepo 内部不再读取
        OrderBy:           req.GetOrderBy(),
        Descending:        req.GetDescending(),
        Pagination:        dbx.Pagination{...},
    }

    // 跨表过滤：service 层先查 object IDs（单表），再传给 fileRepo
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
    // ... 原有后续逻辑 ...
}
```

#### 修改 `service/admin.go::adminListFiles`

```go
func (s *StorageService) adminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
    // ... 解析 filter / vendor ...

    var objectIDs []int64
    needObjectJoin := req.GetContentTypePrefix() != "" || vendor != 0 || req.GetBucket() != ""
    if needObjectJoin {
        ids, err := s.objectRepo.FindIDsByFilter(ctx, req.GetContentTypePrefix(), vendor, req.GetBucket())
        if err != nil { ... }
        if len(ids) == 0 { return empty response }
        objectIDs = ids
    }

    files, total, err := s.fileRepo.ListAll(ctx, filter, objectIDs)
    // ...
}
```

#### 修改 `service/admin.go::adminGetStats`

```go
func (s *StorageService) adminGetStats(ctx context.Context, req *storagev1.AdminGetStatsRequest) (*storagev1.AdminGetStatsResponse, error) {
    stats, err := s.getStorageStats(ctx, int32(req.GetOwnerType()), req.GetOwnerId())
    if err != nil { ... }
    // ... 后续不变 ...
}
```

#### service 层依赖

- `StorageService` 已持有 `db *gorm.DB`（`service.go:33`），通过它构造各 repo：`repository.NewXxxRepo(s.db)`
- 或者使用已注入的 repo 字段（如 `s.fileRepo`, `s.objectRepo`, `s.quotaRepo`）
- 不再需要 `resolveTableName`，因此 `repository/table_name.go` 可删除

## 跨表查询拆解映射

| 原 SQL | 拆解为 | 性能影响 |
|--------|--------|---------|
| `GetPhysicalStats`（object WHERE id IN (subquery on file)） | `FileRepo.FindObjectIDsByOwner` + `ObjectRepo.CountActiveAndSumSizeByIDs` | 2 次单表 vs 1 次嵌套子查询，性能基本持平（DB 优化器对 IN 子查询处理良好） |
| `GetProviderStats`（object ... GROUP BY vendor, with file subquery） | `FileRepo.FindObjectIDsByOwner` + `ObjectRepo.GroupByVendorCountAndSumSizeByIDs` | 同上 |
| `GetBucketObjectStats` | `FileRepo.FindObjectIDsByOwner` + `ObjectRepo.GroupByBucketCountAndSumSizeByIDs` | 同上 |
| `GetOwnerStats`（file JOIN object GROUP BY owner_type） | `FileRepo.FindOwnerObjectIDPairs` + `ObjectRepo.BatchGetByIDs` + 内存按 owner_type 聚合 | 内存聚合替代 DB JOIN；数据量大时内存占用上升 |
| `GetBucketFileStats`（file JOIN object GROUP BY bucket） | `FileRepo.FindObjectIDsByOwner` + `ObjectRepo.BatchGetByIDs` + 内存按 bucket 聚合 | 同上 |
| `GetFileCount`（单表，generated） | 不变 | 无 |
| `GetUsedBytes/GetTotalUsedBytes`（单表，generated） | 不变 | 无 |
| `ListByOwner/ListAll` 内部 ContentTypePrefix 跨表查询 | service 层先调 `ObjectRepo.FindIDsByContentTypePrefix`/`FindIDsByFilter` 再传给 `FileRepo.ListByOwner/ListAll` | 性能基本一致（原本也是两阶段查询） |

## 测试影响

| 测试文件 | 影响 |
|---------|------|
| `repository/upload_session_repo_test.go` | 重命名为 `upload_session_test.go`，无代码改动 |
| `service/service_test.go` | 若有引用 `repository.StatsFilter/GlobalStats/...` 类型，需改 import 为 `service` 包；其余测试无影响 |
| 其他 service / repository 测试 | 检查是否有引用旧类型路径 |

## 验证清单

- [ ] `gofmt -w` + `goimports -w` 全部改动文件
- [ ] `golangci-lint run ./internal/store/... ./internal/service/...`
- [ ] `go test ./internal/store/... ./internal/service/...`
- [ ] `go build ./...`
- [ ] `cmd/migrate/` 跑一次 AutoMigrate 验证 model 没破坏
- [ ] 重新跑 gorm gen 同步 `internal/store/generated/`

## Raw SQL 消除清单

| # | 位置 | 形式 | 处理方式 |
|---|------|------|---------|
| 1 | `stats_queries.go` × 5 处 | `db.Raw(sql).Scan(...)` | 跨表 SQL 拆解为单表方法，service 层组合 |
| 2 | `quota_repo.go:85` `IncrementUsed` | `Where("used_bytes + ? <= total_bytes", bytes)` | 改乐观锁：Get + CAS Update，service 层重试 |
| 3 | `quota_repo.go:144` `AddQuota` | `Where(clause.Expr{SQL: "total_bytes + ? >= 0", ...})` | 同上，乐观锁模式 |
| 4 | `object_repo.go:85` `CreateOrGet` | `clause.Column{{Name: "vendor"}, ...}` 硬编码列名 | 改用 `generated.StorageObject.Vendor.Column()` 等 |
| 5 | `quota_repo.go:54` `CreateIfNotExist` | `clause.Column{{Name: "owner_type"}, ...}` 硬编码列名 | 改用 `generated.Quota.OwnerType.Column()` 等 |

**保留（非 raw SQL）**：
- `file_repo.go:254 Select("*")` — GORM 标准 API，表示更新所有字段（包括零值）
- `quota_repo.go::DecrementUsed` 的 `Where(generated.Quota.UsedBytes.Gte(bytes))` — generated 标准比较运算

## Trade-off

1. **性能**：
   - 大部分跨表 SQL 拆解为 2 次单表查询 + 内存组合，DB 优化器对中等数据量场景表现良好
   - `OwnerStats` / `BucketFileStats` 需要内存聚合全表数据，超大数据量下需要分批处理（实现时可优化）
   - `QuotaRepo.IncrementUsed/AddQuota` 改乐观锁后，每次操作多一次 SELECT；高并发可能 CAS 失败需重试（最多 3 次）
2. **代码迁移量**：
   - 删除约 200 行跨表 SQL
   - 新增约 5 个 ObjectRepo 单表方法 + 2 个 FileRepo 单表方法
   - service 层新增约 200 行（stats 组合 + quota 重试封装）
   - FileRepo API 变更影响 2 个 service 调用点
   - QuotaRepo API 不变（仍返回 error），但 service 层需用 `withQuotaRetry` 包装调用
3. **新增错误码**：`ErrQuotaConcurrentConflict`（用于触发 service 层重试）
4. **`GetActiveByID` 重复**：泛型接口无法部分共享，每个 model 文件各自定义一份（接受）
5. **`models/query.go` 消失**：拆分后每个 model 文件自带 gen 注解，更清晰

## 不在本次范围

- CLAUDE.md 中 `internal/models/` 与实际 `internal/store/models/` 路径不一致 → 单独修文档
- `internal/store/generated/` 由 `gorm gen` 重新生成，本次手动同步结构变化即可
- 性能优化（如 OwnerStats 的大数据量分批处理）→ 实现后再评估

## 关联

**实现计划**：[[services/storage-service/plan/v1/1-store-repo-correspondence|1-store-repo-correspondence]]（待创建）
