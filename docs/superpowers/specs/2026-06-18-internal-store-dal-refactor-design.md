# internal/store 重构：对齐 gorm-cli-development skill

- 日期：2026-06-18
- 分支：feat/audit-logging
- 关联 skill：`ai-kit-studio/skills/gorm-cli-development`

## 1. 目标

把 `internal/store/` 全部对齐新版 `gorm-cli-development` skill：

1. 目录改名：`repository/` → `dal/`，Go package 同步改名为 `dal`
2. DAL 风格：Repo struct + receiver 方法 → 包级函数 + 表前缀名（`(ctx, tx, ...)`）
3. Model struct 全部加 `Storage` 服务前缀（含主表 `StorageObject`，已在范围内）
4. 方法名按 skill §6 规则：去掉服务前缀 `Storage`，仅保留表语义
5. `models/` 里的 gen-annotated interface（`FileQuery`/`ObjectQuery`/`QuotaQuery`）同步加 `Storage` 前缀
6. service 层调用点全部更新，`StorageService` struct 简化为只持 `db *gorm.DB`
7. **把 4 处 builder 表达不了的 Raw SQL（3 个 object 聚合 + 1 个 file 投影）迁移到 Typed Raw SQL**（skill §7）；另 2 处 quota 跨列算术 UPDATE 因 gorm.io/cli v0.2.4 限制（UPDATE 模板无法返回 `RowsAffected`）保留 Raw SQL，补 characterization test + 在 doc comment 文档化原因

## 2. 范围

### 2.1 做

- `internal/store/repository/` → `internal/store/dal/`（git mv，保留历史）
- 5 个 model 文件里的 struct 重命名
- `models/register.go` 的 `AllModels()` 同步更新
- 5 个 dal 文件按包级函数风格重写，方法名按 §4 规则命名
- `dal/filters.go`：集中放 `ListFilesFilter` / `AdminListFilesFilter` / `AuditLogFilter`
- `dal/constants.go`：保留 `MaxBatchSize` / `MaxObjectIDResults`
- `dal/upload_session_test.go`：包名 + 构造调用同步更新
- `internal/service/` 全部 10 个 `.go` 文件 + `service_test.go`：替换所有 `repository.*` 引用为 `dal.*`
- `StorageService` struct 字段精简：移除 `objectRepo/fileRepo/quotaRepo/auditLogRepo/sessionRepo`，保留 `db`
- `DBRecorder` 简化：移除 `repo` 字段，改为持 `db`，内部直接调 `dal.CreateAuditLog`
- 跑 `make generate` 重新生成 `internal/store/generated/`，提交生成结果

### 2.2 不做

- 不写 DB 表名迁移脚本（项目处于开发阶段，AutoMigrate 会直接创建新表 `storage_files` 等，旧空表后续手动清理）
- 不动 `models/` 里非表映射的辅助类型（`MapJSON` / `JSONMap` / `FileCountRow` / `ObjectRefCountRow` / `OwnerObjectIDPair` / `PhysicalStatsRow` / `ProviderStatRow` / `BucketObjectStatRow` / `UsedBytesRow`）—— 它们不是表 struct，skill §3.3 的命名规则不适用
- 不改任何业务逻辑、SQL 语义、索引、约束
- 不动 `cmd/migrate/main.go`、`Makefile` 的 `generate` target
- 不迁移简单的 builder 查询（`Where/Take/Find/Set/Update`、`Count(ctx, "*")` 等）到 Typed Raw SQL —— 这些已经是类型安全的，无收益

## 3. Struct 重命名

| 当前 | 新名 | 文件 | 当前表名 | 新表名 |
|---|---|---|---|---|
| `StorageObject` | `StorageObject` | `models/object.go` | `storage_objects` | `storage_objects` |
| `File` | `StorageFile` | `models/file.go` | `files` | `storage_files` |
| `Quota` | `StorageQuota` | `models/quota.go` | `quotas` | `storage_quotas` |
| `AuditLog` | `StorageAuditLog` | `models/audit_log.go` | `audit_logs` | `storage_audit_logs` |
| `UploadSession` | `StorageUploadSession` | `models/upload_session.go` | `upload_sessions`（显式 `TableName()`） | `storage_upload_sessions`（移除 `TableName()`，靠 GORM 推导） |

`UploadSession` 当前有显式 `TableName() string { return "upload_sessions" }`，其他 4 个没有。重构后统一移除，全部靠 GORM 蛇形复数推导。这是为了让"Model 即 schema"原则在所有 model 上一致。

## 4. 方法命名规则

skill §6 例：`UserLoginLog` struct → `dal.CreateLoginLog`（**去掉服务前缀 `User`**），`User` struct → `dal.CreateUser`。

按此规则应用于 storage-service（服务前缀统一为 `Storage`，方法名一律去掉）：

| 新 struct | 方法前缀 | 例子 |
|---|---|---|
| `StorageObject` | `Object` | `dal.CreateObject` / `dal.GetObjectByID` / `dal.FindObjectByVendorBucketMD5` |
| `StorageFile` | `File` | `dal.CreateFile` / `dal.GetFileByID` / `dal.ListFilesByOwner` |
| `StorageQuota` | `Quota` | `dal.CreateQuota` / `dal.GetQuotaByOwner` / `dal.IncrementQuotaUsed` |
| `StorageAuditLog` | `AuditLog` | `dal.CreateAuditLog` / `dal.ListAuditLogsByOwner` |
| `StorageUploadSession` | `UploadSession` | `dal.CreateUploadSession` / `dal.GetUploadSessionByID` / `dal.MarkUploadSessionConfirmed` |

`dal.` 包名限定已经说明这是 storage-service 的数据访问层，方法名不必再带 `Storage` 前缀。复数语义用复数（`dal.ListFilesByOwner` 返回多个 file，`dal.DeleteFile` 删一个）。

## 5. Dal 文件级迁移

每个 dal 文件一一对应 models 文件。签名模板：

```go
package dal

import (
    "context"
    "gorm.io/gorm"
    "storage-service/internal/store/generated"
    "storage-service/internal/store/models"
    "storage-service/pkg/xcodes"
)

func CreateFile(ctx context.Context, tx *gorm.DB, f *models.StorageFile) error {
    if err := gorm.G[models.StorageFile](tx).Create(ctx, f); err != nil {
        return xcodes.ErrInternal.Wrapf(err, "create file")
    }
    return nil
}

func GetFileByID(ctx context.Context, tx *gorm.DB, id int64) (*models.StorageFile, error) {
    f, err := gorm.G[models.StorageFile](tx).
        Where(generated.StorageFile.ID.Eq(id)).
        Where(generated.StorageFile.DeletedAt.IsNull()).
        Take(ctx)
    if err != nil { /* ... */ }
    return &f, nil
}
```

错误包装沿用现状：`xcodes.ErrInternal.Wrap / Wrapf`，dal 不吞错。

### 5.1 完整方法名映射表

#### `dal/object.go`（原 `repository/object.go`）

| 旧（receiver 上方法） | 新（包级函数） |
|---|---|
| `ObjectRepo.FindByVendorBucketMD5` | `FindObjectByVendorBucketMD5` |
| `ObjectRepo.GetByID` | `GetObjectByID` |
| `ObjectRepo.BatchGetByIDs` | `BatchGetObjectsByIDs` |
| `ObjectRepo.CreateOrGet` | `CreateOrGetObject` |
| `ObjectRepo.IncrRefCount` | `IncrObjectRefCount` |
| `ObjectRepo.DecrRefCount` | `DecrObjectRefCount` |
| `ObjectRepo.DecrRefCountBy` | `DecrObjectRefCountBy` |
| `ObjectRepo.Delete` | `DeleteObject` |
| `ObjectRepo.DeleteZeroRefCount` | `DeleteZeroRefCountObjects` |
| `ObjectRepo.FindPurgeable` | `FindPurgeableObjects` |
| `ObjectRepo.Purge` | `PurgeObject` |
| `ObjectRepo.FindByObjectKey` | `FindObjectByObjectKey` |
| `ObjectRepo.BatchFindObjectKeys` | `BatchFindObjectsByObjectKeys` |
| `ObjectRepo.FindIDsByContentTypePrefix` | `FindObjectIDsByContentTypePrefix` |
| `ObjectRepo.FindIDsByFilter` | `FindObjectIDsByFilter` |
| `ObjectRepo.CountActiveAndSumSizeByIDs` | `CountActiveAndSumObjectSizeByIDs` |
| `ObjectRepo.GroupByVendorCountAndSumSizeByIDs` | `GroupObjectsByVendorAndSumSize` |
| `ObjectRepo.GroupByBucketCountAndSumSizeByIDs` | `GroupObjectsByBucketAndSumSize` |

#### `dal/file.go`（原 `repository/file.go`）

| 旧 | 新 |
|---|---|
| `FileRepo.Create` | `CreateFile` |
| `FileRepo.GetByIDAndOwner` | `GetFileByIDAndOwner` |
| `FileRepo.GetByID` | `GetFileByID` |
| `FileRepo.ListByOwner` | `ListFilesByOwner` |
| `FileRepo.ListAll` | `ListAllFiles` |
| `FileRepo.Update` | `UpdateFile` |
| `FileRepo.Delete` | `DeleteFile` |
| `FileRepo.BatchDelete` | `BatchDeleteFiles` |
| `FileRepo.CountByOwner` | `CountFilesByOwner` |
| `FileRepo.GetObjectRefCountsByOwner` | `GetFileObjectRefCountsByOwner` |
| `FileRepo.DeleteByOwner` | `DeleteFilesByOwner` |
| `FileRepo.FindObjectIDsByOwner` | `FindFileObjectIDsByOwner` |
| `FileRepo.FindOwnerObjectIDPairs` | `FindFileOwnerObjectIDPairs` |

#### `dal/quota.go`（原 `repository/quota.go`）

| 旧 | 新 |
|---|---|
| `QuotaRepo.GetByOwner` | `GetQuotaByOwner` |
| `QuotaRepo.CreateIfNotExist` | `CreateQuotaIfNotExist` |
| `QuotaRepo.IncrementUsed` | `IncrementQuotaUsed` |
| `QuotaRepo.DecrementUsed` | `DecrementQuotaUsed` |
| `QuotaRepo.SetQuota` | `SetQuota` |
| `QuotaRepo.AddQuota` | `AddQuota` |
| `QuotaRepo.DeleteByOwner` | `DeleteQuotaByOwner` |

#### `dal/audit_log.go`（原 `repository/audit_log.go`）

| 旧 | 新 |
|---|---|
| `AuditLogRepo.Create` | `CreateAuditLog` |
| `AuditLogRepo.ListByOwner` | `ListAuditLogsByOwner` |
| `AuditLogRepo.ListAll` | `ListAllAuditLogs` |

#### `dal/upload_session.go`（原 `repository/upload_session.go`）

| 旧 | 新 |
|---|---|
| `UploadSessionRepo.GetByID` | `GetUploadSessionByID` |
| `UploadSessionRepo.FindPendingDedup` | `FindPendingUploadSessionDedup` |
| `UploadSessionRepo.Create` | `CreateUploadSession` |
| `UploadSessionRepo.MarkConfirmed` | `MarkUploadSessionConfirmed` |
| `UploadSessionRepo.MarkCancelled` | `MarkUploadSessionCancelled` |
| `UploadSessionRepo.MarkExpired` | `MarkUploadSessionExpired` |
| `UploadSessionRepo.ListExpiredPending` | `ListExpiredPendingUploadSessions` |
| `UploadSessionRepo.TryAdvisoryLock` | `TryUploadSessionAdvisoryLock` |

`TryUploadSessionAdvisoryLock` 签名：`(ctx, db *gorm.DB, key int64) (release func() error, acquired bool, err error)`。包级函数依然能完成"从 pool 取 conn、上 advisory lock、返回 release 闭包"的工作，逻辑不变。

### 5.2 Filter / Constants

- `dal/filters.go`：`ListFilesFilter`、`AdminListFilesFilter`、`AuditLogFilter` —— 字段不变，仅包路径变
- `dal/constants.go`：`MaxBatchSize`、`MaxObjectIDResults` —— 完全不变

### 5.3 Raw SQL → Typed Raw SQL 迁移（6 处）

每个被迁移的方法在 dal 层依然是包级函数，签名不变；只是实现从"裸 SQL 字符串拼接"换成"调用 generated 模板方法"。调用方零感知。

#### `dal/object.go`

```go
// before
func (r *ObjectRepo) CountActiveAndSumSizeByIDs(ctx context.Context, ids []int64) (models.PhysicalStatsRow, error) {
    var result models.PhysicalStatsRow
    if len(ids) == 0 {
        return result, nil
    }
    err := r.db.WithContext(ctx).
        Model(&models.StorageObject{}).
        Where(generated.StorageObject.ID.In(ids...)).
        Where(generated.StorageObject.DeletedAt.IsNull()).
        Select("COUNT(*) AS total_objects, COALESCE(SUM(size), 0) AS physical_bytes").
        Scan(&result).Error
    if err != nil {
        return result, xcodes.ErrInternal.Wrapf(err, "count and sum size by ids")
    }
    return result, nil
}

// after
func CountActiveAndSumObjectSizeByIDs(ctx context.Context, tx *gorm.DB, ids []int64) (models.PhysicalStatsRow, error) {
    if len(ids) == 0 {
        return models.PhysicalStatsRow{}, nil
    }
    result, err := generated.StorageObjectQuery[models.StorageObject](tx).CountActiveAndSumSize(ctx, ids)
    if err != nil {
        return models.PhysicalStatsRow{}, xcodes.ErrInternal.Wrapf(err, "count and sum size by ids")
    }
    return result, nil
}
```

`GroupObjectsByVendorAndSumSize` / `GroupObjectsByBucketAndSumSize` 同模式，分别调 `GroupByVendorCountAndSumSize` / `GroupByBucketCountAndSumSize`。空切片短路保留在 dal 包装函数里（避免发送 `IN ()` 给 Postgres）。

#### `dal/file.go`

```go
// before
func (r *FileRepo) FindOwnerObjectIDPairs(ctx context.Context) ([]models.OwnerObjectIDPair, error) {
    var pairs []models.OwnerObjectIDPair
    err := r.db.WithContext(ctx).
        Model(&models.File{}).
        Select("owner_type, object_id").
        Where("deleted_at IS NULL").
        Order("id").
        Limit(MaxObjectIDResults).
        Scan(&pairs).Error
    if err != nil {
        return nil, xcodes.ErrInternal.Wrapf(err, "find owner object pairs")
    }
    return pairs, nil
}

// after
func FindFileOwnerObjectIDPairs(ctx context.Context, tx *gorm.DB) ([]models.OwnerObjectIDPair, error) {
    return generated.StorageFileQuery[models.StorageFile](tx).FindOwnerObjectIDPairs(ctx, MaxObjectIDResults)
}
```

错误处理：模板方法返回的 error 是底层 driver error，dal 这里没有"业务可识别"的错误码要映射，直接返回即可（与 before 行为一致：before 里只 wrap 了一次 ErrInternal）。after 也可加一层 `xcodes.ErrInternal.Wrapf` 保持 wrap 一致性 —— **推荐 wrap，统一风格**。

#### `dal/quota.go`

```go
// before
func (r *QuotaRepo) IncrementUsed(ctx context.Context, ownerType int32, ownerID, bytes int64) error {
    rowsAffected, err := gorm.G[models.Quota](r.db).
        Where(generated.Quota.OwnerType.Eq(ownerType)).
        Where(generated.Quota.OwnerID.Eq(ownerID)).
        Where(generated.Quota.DeletedAt.IsNull()).
        Where("used_bytes + ? <= total_bytes", bytes).   // ← 裸 SQL
        Set(generated.Quota.UsedBytes.Incr(bytes)).
        Update(ctx)
    if err != nil {
        return xcodes.ErrInternal.Wrapf(err, "increment used")
    }
    if rowsAffected == 0 {
        return xcodes.ErrQuotaExceeded.New()
    }
    return nil
}

// after
func IncrementQuotaUsed(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, bytes int64) error {
    rowsAffected, err := generated.StorageQuotaQuery[models.StorageQuota](tx).IncrementUsed(ctx, ownerType, ownerID, bytes)
    if err != nil {
        return xcodes.ErrInternal.Wrapf(err, "increment used")
    }
    if rowsAffected == 0 {
        return xcodes.ErrQuotaExceeded.New()
    }
    return nil
}
```

`AddQuota` 同模式，`rowsAffected == 0` 映射到 `xcodes.ErrQuotaInsufficientTotal.New()`，行为与 before 一致。

#### 当前代码里的相关注释要清理

- `repository/object.go` 里 `CountActiveAndSumSizeByIDs` / `GroupByVendorCountAndSumSizeByIDs` / `GroupByBucketCountAndSumSizeByIDs` 上方"raw Select required" 长注释删除（不再适用）
- `repository/file.go` 里 `FindOwnerObjectIDPairs` 上方关于 raw Select 的说明删除
- `repository/quota.go` 里 `IncrementUsed` / `AddQuota` 上方关于"raw SQL cross-column arithmetic required"的长注释删除（仍可保留一句"通过 Typed Raw SQL 表达跨列算术条件"作为上下文）

### 5.4 实际产出 vs 原计划（Task 12 实施时发现）

**已迁移（4 处）**：
- `StorageObjectQuery.CountActiveAndSumSize` / `GroupByVendorCountAndSumSize` / `GroupByBucketCountAndSumSize`
- `StorageFileQuery.FindOwnerObjectIDPairs`

**未迁移（2 处，保留 Raw SQL 形态）**：
- `dal.IncrementQuotaUsed`
- `dal.AddQuota`

**原因**：gorm.io/cli v0.2.4 的 UPDATE 模板 codegen 两条路径都不能用：
- 单返回值 `error` → 内部调 `e.Exec(ctx, ...)`，只返回 error，**无法拿到 RowsAffected**
- 双返回值 `(T, error)` → 内部调 `e.Raw(...).Scan(ctx, &result)`，是 SELECT 语义；对 UPDATE 永远返回零值 `T`

而这两个 dal 方法的语义依赖 `rowsAffected == 0 → ErrQuotaExceeded / ErrQuotaInsufficientTotal`，迁移后会让所有成功 UPDATE 都误判为业务错误。

**保留方案**：
- 函数体保持 Raw SQL `Where("used_bytes + ? <= total_bytes", bytes)` + 类型安全的 `Set(...)` + `Update(ctx)`（builder 仍可拿到 RowsAffected）
- 加 5 个 characterization test（`dal/quota_query_test.go`）锁定行为
- 函数 doc comment 详细记录原因（指向 gorm.io/cli v0.2.4 source code 的 finishMethodBody 两条路径）

**验证过的非阻塞项**：spec §10 风险 5（`@bytes` 在模板里被引用两次）实测无问题 —— gorm.io/cli 把同名占位符绑定一次，生成的 Go 方法签名只有一个 `bytes` 参数。

## 6. Models 包改动

### 6.1 Struct 重命名

5 个文件按 §3 表格改：

- `models/object.go`: `type StorageObject struct {...}` 不变
- `models/file.go`: `type File struct {...}` → `type StorageFile struct {...}`；文件名不变（`file.go`）
- `models/quota.go`: `type Quota struct {...}` → `type StorageQuota struct {...}`
- `models/audit_log.go`: `type AuditLog struct {...}` → `type StorageAuditLog struct {...}`
- `models/upload_session.go`: `type UploadSession struct {...}` → `type StorageUploadSession struct {...}`；**移除** `func (StorageUploadSession) TableName() string` 方法

### 6.2 Interface 改名 + 签名更新 + 新增 Typed Raw SQL 方法

`models/object.go`：
```go
type StorageObjectQuery interface {
    // 已有（保留）
    // SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
    GetActiveByID(id int64) (StorageObject, error)

    // 新增：替代 ObjectRepo.CountActiveAndSumSizeByIDs 里的 db.Model().Select().Scan()
    // SELECT COUNT(*) AS total_objects,
    //        COALESCE(SUM(size), 0) AS physical_bytes
    // FROM @@table
    // WHERE id IN (@ids) AND deleted_at IS NULL
    CountActiveAndSumSize(ids []int64) (PhysicalStatsRow, error)

    // 新增：替代 ObjectRepo.GroupByVendorCountAndSumSizeByIDs
    // SELECT vendor,
    //        COUNT(*) AS object_count,
    //        COALESCE(SUM(size), 0) AS total_bytes
    // FROM @@table
    // WHERE id IN (@ids) AND deleted_at IS NULL
    // GROUP BY vendor
    GroupByVendorCountAndSumSize(ids []int64) ([]ProviderStatRow, error)

    // 新增：替代 ObjectRepo.GroupByBucketCountAndSumSizeByIDs
    // SELECT bucket,
    //        COUNT(*) AS object_count,
    //        COALESCE(SUM(size), 0) AS total_bytes
    // FROM @@table
    // WHERE id IN (@ids) AND deleted_at IS NULL
    // GROUP BY bucket
    GroupByBucketCountAndSumSize(ids []int64) ([]BucketObjectStatRow, error)
}
```

`models/file.go`：
```go
type StorageFileQuery interface {
    // 已有（保留）
    // SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
    GetActiveByID(id int64) (StorageFile, error)

    // 已有（保留）
    // SELECT object_id, COUNT(*) AS count
    // FROM @@table
    // WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
    // GROUP BY object_id
    GetObjectRefCounts(ownerType int32, ownerID int64) ([]ObjectRefCountRow, error)

    // 已有（保留）
    // SELECT COUNT(*) AS count
    // FROM @@table
    // {{where}}
    //   deleted_at IS NULL
    //   {{if ownerType > 0}} AND owner_type = @ownerType {{end}}
    //   {{if ownerID > 0}} AND owner_id = @ownerID {{end}}
    // {{end}}
    GetFileCount(ownerType int32, ownerID int64) (FileCountRow, error)

    // 新增：替代 FileRepo.FindOwnerObjectIDPairs 里的 db.Model().Select().Scan()
    // SELECT owner_type, object_id
    // FROM @@table
    // WHERE deleted_at IS NULL
    // ORDER BY id
    // LIMIT @limit
    FindOwnerObjectIDPairs(limit int) ([]OwnerObjectIDPair, error)
}
```

`models/quota.go`：
```go
type StorageQuotaQuery interface {
    // 已有（保留）
    // SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
    GetActiveByID(id int64) (StorageQuota, error)

    // 已有（保留）
    // SELECT COALESCE(used_bytes, 0) AS used_bytes
    // FROM @@table
    // WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
    GetUsedBytes(ownerType int32, ownerID int64) (UsedBytesRow, error)

    // 已有（保留）
    // SELECT COALESCE(SUM(used_bytes), 0) AS used_bytes
    // FROM @@table
    // WHERE deleted_at IS NULL
    GetTotalUsedBytes() (UsedBytesRow, error)

    // 新增：替代 QuotaRepo.IncrementUsed 里的 Where("used_bytes + ? <= total_bytes", bytes)
    // UPDATE @@table
    // {{set}} used_bytes = used_bytes + @bytes {{end}}
    // {{where}}
    //   owner_type = @ownerType
    //   AND owner_id = @ownerID
    //   AND deleted_at IS NULL
    //   AND used_bytes + @bytes <= total_bytes
    // {{end}}
    // 返回 rows affected（gen 自动生成方法签名为 (int64, error)）
    IncrementUsed(ownerType int32, ownerID int64, bytes int64) (int64, error)

    // 新增：替代 QuotaRepo.AddQuota 里的 Where("total_bytes + ? >= 0", delta)
    // UPDATE @@table
    // {{set}} total_bytes = total_bytes + @delta {{end}}
    // {{where}}
    //   owner_type = @ownerType
    //   AND owner_id = @ownerID
    //   AND deleted_at IS NULL
    //   AND total_bytes + @delta >= 0
    // {{end}}
    AddQuota(ownerType int32, ownerID int64, delta int64) (int64, error)
}
```

辅助行 struct（`FileCountRow` / `ObjectRefCountRow` / `OwnerObjectIDPair` / `PhysicalStatsRow` / `ProviderStatRow` / `BucketObjectStatRow` / `UsedBytesRow`）保持原名 —— 它们是查询结果行，不是表映射。

**gen 模板要点**：
- `@@table` 由 gen 替换为 model 推导出的表名（`storage_objects` / `storage_files` / `storage_quotas`）
- `@ids`、`@ownerType` 等是参数占位符；切片参数（`@ids`）gen 自动展开成 `IN ($1, $2, ...)`，无需手写
- 同一参数（如 `IncrementUsed` 里的 `@bytes`）可在模板中被引用多次，gen 只绑定一次
- `{{where}}…{{end}}` 自动处理前导 `AND`/`OR`；`{{set}}…{{end}}` 自动处理 UPDATE 字段间逗号
- 生成的方法签名：interface 里不写 `ctx`，但调用时第一个参数传 `ctx`（`generated.StorageObjectQuery[models.StorageObject](tx).CountActiveAndSumSize(ctx, ids)`）

### 6.3 register.go

```go
func AllModels() []any {
    return []any{
        &StorageObject{},
        &StorageFile{},
        &StorageQuota{},
        &StorageAuditLog{},
        &StorageUploadSession{},
    }
}
```

`genconfig.Config` 不变。

## 7. Service 层改动

### 7.1 `StorageService` struct（service.go）

```go
type StorageService struct {
    storagev1.UnimplementedStorageServiceServer
    db       *gorm.DB
    ownDB    bool
    redis    *redis.Client
    ownRedis bool
    cron     *cron.Cron
    ownCron  bool
    registry *provider.Registry
    gid      thirdcall.GIDService
    limiter  ratelimit.Limiter
    cfg      *config.Config

    audit     Recorder
    manager   *lifecycle.Manager
    stsCache  *stsCache
    dedupLock UploadDedupLock
}
```

移除：`objectRepo / fileRepo / quotaRepo / auditLogRepo / sessionRepo` 五个字段。所有原本走 `s.objectRepo.Xxx(ctx, ...)` 的调用改成 `dal.XxxObject(ctx, s.db, ...)`。

`New()` 里删除 5 行 `repository.NewXxxRepo(db)` 构造。

### 7.2 调用点改写示例

```go
// before
txFileRepo := repository.NewFileRepo(tx)
txObjRepo := repository.NewObjectRepo(tx)
if err := txFileRepo.Delete(ctx, id); err != nil { return err }
if err := txObjRepo.DecrRefCount(ctx, objID); err != nil { return err }

// after
if err := dal.DeleteFile(ctx, tx, id); err != nil { return err }
if err := dal.DecrObjectRefCount(ctx, tx, objID); err != nil { return err }
```

```go
// before
obj, err := s.objectRepo.GetByID(ctx, id)

// after
obj, err := dal.GetObjectByID(ctx, s.db, id)
```

```go
// before
if len(req.GetFileIds()) > repository.MaxBatchSize { ... }

// after
if len(req.GetFileIds()) > dal.MaxBatchSize { ... }
```

```go
// before
filter := repository.ListFilesFilter{...}

// after
filter := dal.ListFilesFilter{...}
```

### 7.3 `DBRecorder`（audit.go）

```go
type DBRecorder struct {
    db  *gorm.DB
    gid thirdcall.GIDService
}

func NewDBRecorder(db *gorm.DB, gid thirdcall.GIDService) *DBRecorder {
    return &DBRecorder{db: db, gid: gid}
}
```

内部 `recordInTx` 等方法把 `repository.NewAuditLogRepo(tx).Create(ctx, log)` 改成 `dal.CreateAuditLog(ctx, tx, log)`。

### 7.4 文件清单（10 个 service 文件 + 1 个 test）

需要改 import 和调用点的文件：
- `internal/service/admin.go`
- `internal/service/audit.go`
- `internal/service/cancel_upload.go`
- `internal/service/cleanup.go`
- `internal/service/file.go`
- `internal/service/quota.go`
- `internal/service/service.go`
- `internal/service/upload.go`
- `internal/service/upload_gc.go`
- `internal/service/service_test.go`

## 8. Generated 代码

跑 `make generate`（即 `gorm gen -i ./internal/store/models -o ./internal/store/generated`）会重新生成所有 `.gen.go` 文件，导出名变化：

- `generated.File` → `generated.StorageFile`
- `generated.Quota` → `generated.StorageQuota`
- `generated.AuditLog` → `generated.StorageAuditLog`
- `generated.UploadSession` → `generated.StorageUploadSession`
- `generated.StorageObject` 不变

同时 `FileQuery` → `StorageFileQuery` 等。dal 文件里所有 `generated.Xxx.Yyy` 引用自然吸收这些变化（dal 文件本来就在重写）。

## 9. 验证

```bash
# 1. build
go build ./...

# 2. regenerate and check no drift
make generate && git diff --exit-code internal/store/generated

# 3. format / lint
gofmt -w .
goimports -w .
golangci-lint run ./...

# 4. tests
go test -race -coverprofile=coverage.out ./internal/store/dal/... ./internal/service/...

# 5. verify StorageUploadSession no longer has TableName()
grep -Rn 'func (StorageUploadSession) TableName' internal/store/models && \
  echo "FAIL: TableName still present" || echo "OK"

# 6. verify no remaining raw Select().Scan() in dal
grep -RnE '\.Select\(' internal/store/dal && \
  echo "WARN: Select calls remain in dal (review manually)" || echo "OK"

# 7. verify no remaining raw Where(\"...\") in dal
grep -RnE 'Where\("' internal/store/dal && \
  echo "WARN: raw Where strings remain in dal (review manually)" || echo "OK"
```

集成测试 `service_test.go` 应全部通过（含 `service_test.go:886` 的那段集成测试）。

### 9.1 新增的 Typed Raw SQL 模板测试

为 6 个新模板方法补集成测试（在 `dal/` 下，沿用 `upload_session_test.go` 的 `dbx.SetupTestDB` + AutoMigrate 模式）：

- `dal/object_query_test.go`：`CountActiveAndSumSize` / `GroupByVendorCountAndSumSize` / `GroupByBucketCountAndSumSize` 各覆盖（空 ids 短路、命中、跨 vendor/bucket 分组、软删除过滤）
- `dal/file_query_test.go`：`FindOwnerObjectIDPairs` 覆盖（含 LIMIT 边界、按 id 排序、软删除过滤）
- `dal/quota_query_test.go`：`IncrementUsed` 覆盖（成功、超 quota 拒绝、owner 不存在、多次累加）；`AddQuota` 覆盖（成功、负数 refund 接近零边界、refund 过大拒绝）

每个 case 对照旧实现的行为跑一遍，确保 SQL 等价。

## 10. 风险与回退

- 风险 1：dal 函数包级化后，service 层 `db vs tx` 传参容易混。缓解：所有 dal 函数第二参数统一命名 `tx *gorm.DB`（即便传的是非事务 `s.db`），从命名上提示调用方这是 GORM 句柄。
- 风险 2：表名从 `files` → `storage_files` 等是 breaking change，本地 / staging 数据需手动清掉旧表（`DROP TABLE files` 等）或重建 schema。开发阶段影响可控。
- 风险 3：`gorm gen` 重生成可能出现新的字段辅助器 API 变化（gorm.io/cli 升级带来的），需对照旧 generated 文件 diff 确认只动了我们引起的部分。
- 风险 4：Typed Raw SQL 模板写错（特别是 `IN (@ids)` 展开和 `GROUP BY` 列名）可能让 SQL 语义偏离原实现。缓解：每个新模板都配集成测试（§9.1），对照旧行为跑一遍。
- 风险 5：`IncrementUsed` / `AddQuota` 模板里 `@bytes` / `@delta` 被引用两次（一次进 SET，一次进 WHERE），如果 gen 把它当成两个独立参数会绑定两次但语义等价；如果 gen 要求唯一参数名则需改名。验证手段：生成后看 `.gen.go` 方法签名，确认入参只有一个。
- 回退：所有改动在单一分支，整体 reset 即可。无外部依赖变更。

## 关联

- skill 文件：`/Users/moss/code/base/ai-kit-studio/skills/gorm-cli-development/gorm-cli-development.md`
- skill 骨架：`/Users/moss/code/base/ai-kit-studio/skills/gorm-cli-development/skeleton/`
