# internal/store Dal Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `internal/store/` 完全对齐新版 `gorm-cli-development` skill：目录改名、Repo struct 改包级函数、所有 model struct 加 `Storage` 前缀、Raw SQL 迁移到 Typed Raw SQL 模板。

**Architecture:** 分阶段推进。Phase 1-3 是机械重命名（struct 改名、interface 改名、目录改名），每阶段后 build 保持绿。Phase 4 是行为重构（Repo struct → 包级函数，per-table），每个表一个独立提交。Phase 5 是 Raw SQL → Typed Raw SQL 模板迁移（TDD：先写测试）。Phase 6 是收尾验证。

**Tech Stack:** Go 1.x，GORM + gorm.io/cli（gorm gen），PostgreSQL，testcontainers（dbx.SetupTestDB），xerr 错误码。

**关联 spec：** `docs/superpowers/specs/2026-06-18-internal-store-dal-refactor-design.md`

**目标分支：** `feat/audit-logging`（直接在当前分支推进，不开 worktree）

**前提条件：**
- `gorm` CLI 已安装：`go install gorm.io/cli/gorm@latest`
- 本地有 Docker（testcontainers 需要）
- 跑过 `make test`，当前 baseline 全绿

**全局约定（每个 task 都遵守）：**
- DAL 包级函数第二参数统一命名 `tx *gorm.DB`（即便传非事务 `s.db`），从命名提示这是 GORM 句柄
- 错误包装沿用现状：`xcodes.ErrInternal.Wrap / Wrapf`
- 每步验证：先 `go build ./...`，再 `go test ./internal/store/... ./internal/service/...`，绿了才提交
- 提交信息用英文，遵循 Conventional Commits

---

## Phase 1: Model struct 改名（机械重命名）

### Task 1: 把 File/Quota/AuditLog/UploadSession 改成 Storage 前缀，regen，更新所有 caller

**Files:**
- Modify: `internal/store/models/file.go`
- Modify: `internal/store/models/quota.go`
- Modify: `internal/store/models/audit_log.go`
- Modify: `internal/store/models/upload_session.go`
- Modify: `internal/store/models/register.go`
- Modify: `internal/store/repository/object.go`、`file.go`、`quota.go`、`audit_log.go`、`upload_session.go`、`upload_session_test.go`
- Modify: `internal/service/admin.go`、`audit.go`、`cancel_upload.go`、`cleanup.go`、`file.go`、`helpers.go`、`quota.go`、`service.go`、`service_test.go`、`stats.go`、`upload.go`、`upload_gc.go`、`upload_gc_test.go`
- Regen: `internal/store/generated/*.gen.go`

- [ ] **Step 1: 改 models/file.go 的 struct 名**

打开 `internal/store/models/file.go`，把：

```go
type File struct {
```

改成：

```go
type StorageFile struct {
```

同时把文件内所有引用 `File` 的地方（如 `FileQuery` interface 里的返回类型 `(File, error)`、辅助行 struct 注释里"`FileRepo`"）改成 `StorageFile`。

注意：`FileCountRow`、`ObjectRefCountRow`、`OwnerObjectIDPair`、`FileQuery` 这几个标识符**保持原名**（不是表 struct，spec §2.2 说明）。

- [ ] **Step 2: 改 models/quota.go 的 struct 名**

把 `type Quota struct {` 改成 `type StorageQuota struct {`。`QuotaQuery` interface 里的返回类型 `(Quota, error)` 改成 `(StorageQuota, error)`。`UsedBytesRow`、`QuotaQuery` 保持原名。

- [ ] **Step 3: 改 models/audit_log.go 的 struct 名**

把 `type AuditLog struct {` 改成 `type StorageAuditLog struct {`。`JSONMap` 保持原名。

- [ ] **Step 4: 改 models/upload_session.go 的 struct 名 + 移除 TableName()**

把 `type UploadSession struct {` 改成 `type StorageUploadSession struct {`。

删除文件末尾的：

```go
// TableName overrides the default table name.
func (UploadSession) TableName() string { return "upload_sessions" }
```

整段删掉。

- [ ] **Step 5: 改 models/register.go 的 AllModels()**

把：

```go
func AllModels() []any {
    return []any{
        &StorageObject{},
        &File{},
        &Quota{},
        &AuditLog{},
        &UploadSession{},
    }
}
```

改成：

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

- [ ] **Step 6: 跑 gorm gen 重新生成**

```bash
make generate
```

期望：`internal/store/generated/` 下的 `file.gen.go`、`quota.gen.go`、`audit_log.gen.go`、`upload_session.gen.go` 里的导出名跟着改成 `StorageFile`、`StorageQuota`、`StorageAuditLog`、`StorageUploadSession`。`object.gen.go` 不变。

- [ ] **Step 7: 用 sed 批量更新 repository/ 包里的引用**

repository/ 里需要替换的标识符：

```bash
# 注意顺序：先生成的不影响后面的
# 在 internal/store/repository/ 下：
sed -i '' 's/\bmodels\.File\b/models.StorageFile/g' internal/store/repository/*.go
sed -i '' 's/\bmodels\.Quota\b/models.StorageQuota/g' internal/store/repository/*.go
sed -i '' 's/\bmodels\.AuditLog\b/models.StorageAuditLog/g' internal/store/repository/*.go
sed -i '' 's/\bmodels\.UploadSession\b/models.StorageUploadSession/g' internal/store/repository/*.go
sed -i '' 's/\bgenerated\.File\b/generated.StorageFile/g' internal/store/repository/*.go
sed -i '' 's/\bgenerated\.Quota\b/generated.StorageQuota/g' internal/store/repository/*.go
sed -i '' 's/\bgenerated\.AuditLog\b/generated.StorageAuditLog/g' internal/store/repository/*.go
sed -i '' 's/\bgenerated\.UploadSession\b/generated.StorageUploadSession/g' internal/store/repository/*.go
```

注意：`\b` 在 macOS sed 不支持，需要用 `[[:<:]]`/`[[:>:]]` 或干脆手改。建议改完后人工 spot-check `git diff`。

- [ ] **Step 8: 用 sed 批量更新 service/ 包里的引用**

```bash
sed -i '' 's/models\.File\b/models.StorageFile/g' internal/service/*.go
sed -i '' 's/models\.Quota\b/models.StorageQuota/g' internal/service/*.go
sed -i '' 's/models\.AuditLog\b/models.StorageAuditLog/g' internal/service/*.go
sed -i '' 's/models\.UploadSession\b/models.StorageUploadSession/g' internal/service/*.go
sed -i '' 's/generated\.File\b/generated.StorageFile/g' internal/service/*.go
sed -i '' 's/generated\.Quota\b/generated.StorageQuota/g' internal/service/*.go
sed -i '' 's/generated\.AuditLog\b/generated.StorageAuditLog/g' internal/service/*.go
sed -i '' 's/generated\.UploadSession\b/generated.StorageUploadSession/g' internal/service/*.go
```

注意 macOS BSD sed 的 `\b` 行为不一致 —— 最稳妥用 `perl -i -pe 's/\bmodels\.File\b/models.StorageFile/g' internal/service/*.go`。

- [ ] **Step 9: build + test 验证**

```bash
go build ./...
go test -count=1 ./internal/store/repository/... ./internal/service/...
```

期望：build 通过，test 全绿。如果 sed 漏改，编译器会报"undefined: models.File"之类的错，按提示手改。

- [ ] **Step 10: 提交**

```bash
git add internal/store/models/ internal/store/generated/ internal/store/repository/ internal/service/
git commit -m "refactor(models): prefix all storage-service structs with Storage

Rename File→StorageFile, Quota→StorageQuota, AuditLog→StorageAuditLog,
UploadSession→StorageUploadSession per gorm-cli-development §3.3 service-
prefix rule. Remove explicit TableName() on StorageUploadSession so all
models consistently use GORM's snake+plural derivation. Regen generated/."
```

---

## Phase 2: gen-annotated interface 改名（机械重命名）

### Task 2: FileQuery/QuotaQuery/ObjectQuery → StorageXxxQuery，更新 generated + caller

**Files:**
- Modify: `internal/store/models/file.go`、`quota.go`、`object.go`
- Modify: `internal/store/repository/file.go`、`quota.go`、`object.go`
- Modify: `internal/service/stats.go`
- Regen: `internal/store/generated/`

- [ ] **Step 1: 改 models 里的 interface 名**

`models/file.go`：`type FileQuery interface` → `type StorageFileQuery interface`

`models/quota.go`：`type QuotaQuery interface` → `type StorageQuotaQuery interface`

`models/object.go`：`type ObjectQuery interface` → `type StorageObjectQuery interface`

- [ ] **Step 2: 跑 gorm gen**

```bash
make generate
```

`generated/` 里对应的 query 类型跟着改名。

- [ ] **Step 3: 替换所有 caller**

```bash
# repository/ 和 service/ 里
perl -i -pe 's/\bFileQuery\b/StorageFileQuery/g' internal/store/repository/*.go internal/service/*.go
perl -i -pe 's/\bQuotaQuery\b/StorageQuotaQuery/g' internal/store/repository/*.go internal/service/*.go
perl -i -pe 's/\bObjectQuery\b/StorageObjectQuery/g' internal/store/repository/*.go internal/service/*.go
```

- [ ] **Step 4: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/repository/... ./internal/service/...
```

期望：全绿。

- [ ] **Step 5: 提交**

```bash
git add internal/store/models/ internal/store/generated/ internal/store/repository/ internal/service/
git commit -m "refactor(models): prefix gen-annotated query interfaces with Storage

FileQuery→StorageFileQuery, QuotaQuery→StorageQuotaQuery,
ObjectQuery→StorageObjectQuery. Names now match their owning struct.
Regen generated/."
```

---

## Phase 3: 目录改名 repository/ → dal/（机械重命名）

### Task 3: git mv，改 package 声明，更新所有 import

**Files:**
- Rename: `internal/store/repository/` → `internal/store/dal/`
- Modify: 7 个 .go 文件的 package 声明
- Modify: 13 个 service 文件的 import 路径

- [ ] **Step 1: git mv 目录**

```bash
git mv internal/store/repository internal/store/dal
```

- [ ] **Step 2: 改 package 声明**

```bash
sed -i '' 's/^package repository$/package dal/' internal/store/dal/*.go
```

影响文件：`object.go`、`file.go`、`quota.go`、`audit_log.go`、`upload_session.go`、`upload_session_test.go`、`constants.go`。

- [ ] **Step 3: 改 service 层 import**

```bash
sed -i '' 's|"storage-service/internal/store/repository"|"storage-service/internal/store/dal"|g' internal/service/*.go
```

注意：service 文件里的 `repository.XxxRepo` 类型引用 **本次不改**（仍然存在 Repo struct），Phase 4 才改。仅改 import 路径。

- [ ] **Step 4: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

期望：全绿。如果 sed 漏改，编译器会报"repository (unresolved import)"。

- [ ] **Step 5: 提交**

```bash
git add internal/store/dal/ internal/service/
git commit -m "refactor(store): rename repository/ → dal/ per skill convention

Pure rename + package decl + import path update. Repo struct style and
method bodies unchanged. Behavioral refactor to package-level functions
follows in subsequent commits."
```

---

## Phase 4: Repo struct → 包级函数（行为重构，per-table）

每个 task 处理一个表，原子提交。每个 task 完成后 build 绿、test 绿。

### Task 4: dal/object.go → 包级函数

**Files:**
- Modify: `internal/store/dal/object.go`
- Modify: `internal/service/service.go`、`admin.go`、`cleanup.go`、`file.go`

参考 spec §5.1 的 Object 方法映射表，把 18 个 receiver 方法转成包级函数。

- [ ] **Step 1: 删除 ObjectRepo struct 和构造函数**

打开 `internal/store/dal/object.go`，删除：

```go
type ObjectRepo struct {
    db *gorm.DB
}

func NewObjectRepo(db *gorm.DB) *ObjectRepo {
    return &ObjectRepo{db: db}
}
```

- [ ] **Step 2: 逐个改写 18 个方法为包级函数**

按 spec §5.1 的映射表，每个 receiver 方法 `func (r *ObjectRepo) Xxx(ctx context.Context, ...) ...` 改成 `func XxxObject(ctx context.Context, tx *gorm.DB, ...) ...`。函数体里 `r.db` 全部换成 `tx`。

例子（FindObjectByVendorBucketMD5）：

```go
// before
func (r *ObjectRepo) FindByVendorBucketMD5(ctx context.Context, vendor int32, bucket, md5 string) (*models.StorageObject, bool, error) {
    obj, err := gorm.G[models.StorageObject](r.db).
        Where(generated.StorageObject.Vendor.Eq(vendor)).
        ...
}

// after
func FindObjectByVendorBucketMD5(ctx context.Context, tx *gorm.DB, vendor int32, bucket, md5 string) (*models.StorageObject, bool, error) {
    obj, err := gorm.G[models.StorageObject](tx).
        Where(generated.StorageObject.Vendor.Eq(vendor)).
        ...
}
```

18 个方法名逐一按 spec §5.1 映射表改名：
- `FindByVendorBucketMD5` → `FindObjectByVendorBucketMD5`
- `GetByID` → `GetObjectByID`
- `BatchGetByIDs` → `BatchGetObjectsByIDs`
- `CreateOrGet` → `CreateOrGetObject`
- `IncrRefCount` → `IncrObjectRefCount`
- `DecrRefCount` → `DecrObjectRefCount`
- `DecrRefCountBy` → `DecrObjectRefCountBy`
- `Delete` → `DeleteObject`
- `DeleteZeroRefCount` → `DeleteZeroRefCountObjects`
- `FindPurgeable` → `FindPurgeableObjects`
- `Purge` → `PurgeObject`
- `FindByObjectKey` → `FindObjectByObjectKey`
- `BatchFindObjectKeys` → `BatchFindObjectsByObjectKeys`
- `FindIDsByContentTypePrefix` → `FindObjectIDsByContentTypePrefix`
- `FindIDsByFilter` → `FindObjectIDsByFilter`
- `CountActiveAndSumSizeByIDs` → `CountActiveAndSumObjectSizeByIDs`
- `GroupByVendorCountAndSumSizeByIDs` → `GroupObjectsByVendorAndSumSize`
- `GroupByBucketCountAndSumSizeByIDs` → `GroupObjectsByBucketAndSumSize`

注意：聚合方法（最后 3 个）**本次仍保留 Raw SQL 形态**（`db.Model().Select().Scan()`），Phase 5 才迁移到模板。函数体里 `r.db.WithContext(ctx)` 改成 `tx.WithContext(ctx)`。

- [ ] **Step 3: 从 StorageService struct 移除 objectRepo 字段**

`internal/service/service.go` 里：

```go
// before
type StorageService struct {
    ...
    objectRepo   *dal.ObjectRepo
    fileRepo     *dal.FileRepo
    ...
}

// after（仅删 objectRepo 行，其他暂留）
type StorageService struct {
    ...
    fileRepo     *dal.FileRepo
    ...
}
```

并删除 `New()` 函数里的 `objectRepo := dal.NewObjectRepo(db)` 一行，以及 struct 字面量里的 `objectRepo: objectRepo,` 一行。

- [ ] **Step 4: 改 service/admin.go 的 caller**

把所有：

```go
txObjRepo := dal.NewObjectRepo(tx)
// ...
if err := txObjRepo.Xxx(ctx, ...); err != nil { ... }
```

改成：

```go
if err := dal.XxxObject(ctx, tx, ...); err != nil { ... }
```

逐个对照 spec §5.1 的映射表替换。

- [ ] **Step 5: 改 service/cleanup.go 的 caller**

同 Step 4 模式。cleanup.go 里 `objectTx := dal.NewObjectRepo(tx)` 改成直接调 `dal.XxxObject(ctx, tx, ...)`。

- [ ] **Step 6: 改 service/file.go 的 caller**

同 Step 4 模式。file.go 里多处 `txObjRepo := dal.NewObjectRepo(tx)`，全部展开。

- [ ] **Step 7: 改 service/service.go 里 s.objectRepo 的所有引用**

grep 一下：

```bash
grep -n 's\.objectRepo' internal/service/*.go
```

每个 `s.objectRepo.Xxx(ctx, ...)` 改成 `dal.XxxObject(ctx, s.db, ...)`。

- [ ] **Step 8: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

期望：全绿。

- [ ] **Step 9: 提交**

```bash
git add internal/store/dal/object.go internal/service/
git commit -m "refactor(dal): convert ObjectRepo to package-level functions

Per skill §6, replace ObjectRepo struct + receiver methods with package-
level functions named with the table prefix (FindObjectByVendorBucketMD5,
GetObjectByID, ...). Callers in service/{service,admin,cleanup,file}.go
updated. StorageService.objectRepo field removed.

Raw SQL aggregations (CountActiveAndSumObjectSizeByIDs, ...) preserved
in current db.Model().Select().Scan() form; will migrate to Typed Raw
SQL in a later commit."
```

---

### Task 5: dal/file.go → 包级函数

**Files:**
- Modify: `internal/store/dal/file.go`
- Modify: `internal/service/service.go`、`admin.go`、`file.go`

- [ ] **Step 1: 删除 FileRepo struct 和构造函数**

```go
// 删除
type FileRepo struct { db *gorm.DB }
func NewFileRepo(db *gorm.DB) *FileRepo { return &FileRepo{db: db} }
```

- [ ] **Step 2: 改写 13 个方法为包级函数**

按 spec §5.1 的 File 方法映射表，13 个 receiver 方法转包级函数，名字按规则改：
- `Create` → `CreateFile`
- `GetByIDAndOwner` → `GetFileByIDAndOwner`
- `GetByID` → `GetFileByID`
- `ListByOwner` → `ListFilesByOwner`
- `ListAll` → `ListAllFiles`
- `Update` → `UpdateFile`
- `Delete` → `DeleteFile`
- `BatchDelete` → `BatchDeleteFiles`
- `CountByOwner` → `CountFilesByOwner`
- `GetObjectRefCountsByOwner` → `GetFileObjectRefCountsByOwner`
- `DeleteByOwner` → `DeleteFilesByOwner`
- `FindObjectIDsByOwner` → `FindFileObjectIDsByOwner`
- `FindOwnerObjectIDPairs` → `FindFileOwnerObjectIDPairs`

函数体里 `r.db` 换 `tx`。`FindFileOwnerObjectIDPairs` **本次保留 Raw SQL 形态**，Phase 5 才迁移。

注意：`ListFilesFilter`、`AdminListFilesFilter` 类型暂时留在 file.go 里，Task 9 才集中移到 filters.go。

- [ ] **Step 3: 从 StorageService struct 移除 fileRepo 字段**

`service.go` 里删 `fileRepo *dal.FileRepo` 行，删 `New()` 里 `fileRepo := dal.NewFileRepo(db)` 行，删 struct 字面量里 `fileRepo: fileRepo,` 行。

- [ ] **Step 4: 改 service/admin.go 的 caller**

把 `txFileRepo := dal.NewFileRepo(tx); txFileRepo.Xxx(...)` 展开成 `dal.XxxFile(ctx, tx, ...)`。`dal.NewFileRepo(db)` 同理。

注意 admin.go 里有 `dal.AdminListFilesFilter{...}`、`repository.NewFileRepo(tx).CountByOwner(...)` 等多种调用模式。

- [ ] **Step 5: 改 service/file.go 的 caller**

同 Step 4。file.go 里多处使用 `txFileRepo`。

- [ ] **Step 6: 改 service/service.go 里 s.fileRepo 的引用**

```bash
grep -n 's\.fileRepo' internal/service/*.go
```

逐个改成 `dal.XxxFile(ctx, s.db, ...)`。

- [ ] **Step 7: 改 service/file.go 里 fileCount 等 repository.MaxBatchSize 引用**

```bash
grep -n 'dal\.MaxBatchSize\|repository\.MaxBatchSize' internal/service/*.go
```

应该已经是 `dal.MaxBatchSize`（Task 3 改过）。如果没有，补一下。

- [ ] **Step 8: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

期望：全绿。

- [ ] **Step 9: 提交**

```bash
git add internal/store/dal/file.go internal/service/
git commit -m "refactor(dal): convert FileRepo to package-level functions

13 methods converted to package functions per skill §6. StorageService.
fileRepo field removed. Callers in service/{service,admin,file}.go
updated. FindFileOwnerObjectIDPairs still uses raw Select().Scan();
will migrate to Typed Raw SQL later."
```

---

### Task 6: dal/quota.go → 包级函数

**Files:**
- Modify: `internal/store/dal/quota.go`
- Modify: `internal/service/service.go`、`admin.go`、`quota.go`、`cleanup.go`

- [ ] **Step 1: 删除 QuotaRepo struct 和构造函数**

- [ ] **Step 2: 改写 7 个方法为包级函数**

按 spec §5.1 Quota 映射表：
- `GetByOwner` → `GetQuotaByOwner`
- `CreateIfNotExist` → `CreateQuotaIfNotExist`
- `IncrementUsed` → `IncrementQuotaUsed`（**本次保留 `Where("used_bytes + ? <= total_bytes", bytes)` Raw SQL**，Phase 5 才迁移）
- `DecrementUsed` → `DecrementQuotaUsed`
- `SetQuota` → `SetQuota`
- `AddQuota` → `AddQuota`（**本次保留 Raw SQL**）
- `DeleteByOwner` → `DeleteQuotaByOwner`

函数体里 `r.db` 换 `tx`。注意保留所有 `xcodes.ErrXxx` 错误映射。

- [ ] **Step 3: 从 StorageService struct 移除 quotaRepo 字段**

- [ ] **Step 4: 改 service/quota.go 的 caller**

```bash
grep -n 'quotaRepo\|QuotaRepo\|dal\.NewQuotaRepo' internal/service/*.go
```

`s.quotaRepo.Xxx(ctx, ...)` → `dal.XxxQuota(ctx, s.db, ...)`。

- [ ] **Step 5: 改 service/admin.go 的 caller**

`dal.NewQuotaRepo(tx).DeleteByOwner(...)` → `dal.DeleteQuotaByOwner(ctx, tx, ...)`。

- [ ] **Step 6: 改 service/cleanup.go 的 caller**

`quotaTx := dal.NewQuotaRepo(tx)` → 直接调 `dal.XxxQuota(ctx, tx, ...)`。

- [ ] **Step 7: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

- [ ] **Step 8: 提交**

```bash
git add internal/store/dal/quota.go internal/service/
git commit -m "refactor(dal): convert QuotaRepo to package-level functions

7 methods converted per skill §6. StorageService.quotaRepo field
removed. IncrementQuotaUsed / AddQuota still use raw Where(\"col + ? <=
col\", n) form; will migrate to Typed Raw SQL templates later."
```

---

### Task 7: dal/audit_log.go → 包级函数 + DBRecorder 简化

**Files:**
- Modify: `internal/store/dal/audit_log.go`
- Modify: `internal/service/service.go`、`audit.go`
- Modify: `internal/service/service_test.go`

- [ ] **Step 1: 删除 AuditLogRepo struct 和构造函数**

- [ ] **Step 2: 改写 3 个方法为包级函数**

按 spec §5.1 AuditLog 映射表：
- `Create` → `CreateAuditLog`
- `ListByOwner` → `ListAuditLogsByOwner`
- `ListAll` → `ListAllAuditLogs`

注意 `AuditLogFilter` 类型暂时留在 audit_log.go 里，Task 9 才集中移到 filters.go。

- [ ] **Step 3: 从 StorageService struct 移除 auditLogRepo 字段**

service.go 里删 `auditLogRepo *dal.AuditLogRepo` 字段，删 `New()` 里 `auditLogRepo := dal.NewAuditLogRepo(db)` 行，删 struct 字面量 `auditLogRepo: auditLogRepo,` 行。

- [ ] **Step 4: 简化 DBRecorder**

`internal/service/audit.go` 里：

```go
// before
type DBRecorder struct {
    repo *dal.AuditLogRepo
    gid  thirdcall.GIDService
}

func NewDBRecorder(repo *dal.AuditLogRepo, gid thirdcall.GIDService) *DBRecorder {
    return &DBRecorder{repo: repo, gid: gid}
}
```

改成：

```go
// after
type DBRecorder struct {
    db  *gorm.DB
    gid thirdcall.GIDService
}

func NewDBRecorder(db *gorm.DB, gid thirdcall.GIDService) *DBRecorder {
    return &DBRecorder{db: db, gid: gid}
}
```

`recordInTx` 等方法里把 `dal.NewAuditLogRepo(tx).Create(ctx, log)` 改成 `dal.CreateAuditLog(ctx, tx, log)`，把 `r.repo` 引用改成 `r.db`。

- [ ] **Step 5: 改 service.go 里 NewDBRecorder 的调用**

```go
// before
auditRecorder := NewDBRecorder(auditLogRepo, gidGen)
// after（auditLogRepo 已不存在）
auditRecorder := NewDBRecorder(db, gidGen)
```

- [ ] **Step 6: 改 service/audit.go 里 listMyAuditLogs / adminListAuditLogs**

```bash
grep -n 'auditLogRepo\|dal\.NewAuditLogRepo' internal/service/audit.go
```

`s.auditLogRepo.ListByOwner(...)` → `dal.ListAuditLogsByOwner(ctx, s.db, ...)`。
`s.auditLogRepo.ListAll(...)` → `dal.ListAllAuditLogs(ctx, s.db, ...)`。

- [ ] **Step 7: 改 service/service_test.go**

```bash
grep -n 'auditLogRepo\|dal\.NewAuditLogRepo' internal/service/service_test.go
```

test helper 里 `auditLogRepo := dal.NewAuditLogRepo(db)` 删除，调用 `dal.XxxAuditLog(ctx, db, ...)` 直接调。注意 test 里 `svc.auditLogRepo.ListByOwner(...)` 也要改。

- [ ] **Step 8: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

- [ ] **Step 9: 提交**

```bash
git add internal/store/dal/audit_log.go internal/service/
git commit -m "refactor(dal): convert AuditLogRepo to package-level functions

3 methods converted. DBRecorder simplified to hold *gorm.DB instead of
*AuditLogRepo (per skill §8 — service-layer struct holds db, dal methods
receive tx). StorageService.auditLogRepo field removed."
```

---

### Task 8: dal/upload_session.go → 包级函数

**Files:**
- Modify: `internal/store/dal/upload_session.go`
- Modify: `internal/store/dal/upload_session_test.go`
- Modify: `internal/service/service.go`、`cancel_upload.go`、`upload.go`、`upload_gc.go`
- Modify: `internal/service/service_test.go`、`upload_gc_test.go`

- [ ] **Step 1: 删除 UploadSessionRepo struct 和构造函数**

- [ ] **Step 2: 改写 8 个方法为包级函数**

按 spec §5.1 UploadSession 映射表：
- `GetByID` → `GetUploadSessionByID`
- `FindPendingDedup` → `FindPendingUploadSessionDedup`
- `Create` → `CreateUploadSession`
- `MarkConfirmed` → `MarkUploadSessionConfirmed`
- `MarkCancelled` → `MarkUploadSessionCancelled`
- `MarkExpired` → `MarkUploadSessionExpired`
- `ListExpiredPending` → `ListExpiredPendingUploadSessions`
- `TryAdvisoryLock` → `TryUploadSessionAdvisoryLock`

`TryUploadSessionAdvisoryLock` 签名：`(ctx context.Context, db *gorm.DB, key int64) (release func() error, acquired bool, err error)`。函数体把 `r.db` 换 `db`（不用 tx 命名，因为它取的是底层 `*sql.DB` 的 conn，不是事务）。

- [ ] **Step 3: 从 StorageService struct 移除 sessionRepo 字段**

service.go 删 `sessionRepo *dal.UploadSessionRepo` 字段，删 `New()` 里 `sessionRepo := dal.NewUploadSessionRepo(db)` 行，删 struct 字面量 `sessionRepo: sessionRepo,` 行。

- [ ] **Step 4: 改 dal/upload_session_test.go**

```bash
grep -n 'NewUploadSessionRepo\|UploadSessionRepo' internal/store/dal/upload_session_test.go
```

test 里 `repo := NewUploadSessionRepo(db)` 删除，`repo.Xxx(ctx, ...)` 改成 `XxxUploadSession(ctx, db, ...)` 直接调。包名已经是 `dal`，函数直接可见。

`models.StorageUploadSession{...}` 字面量 Task 1 已改完。

- [ ] **Step 5: 改 service/cancel_upload.go 的 caller**

`dal.NewUploadSessionRepo(tx).MarkCancelled(ctx, ...)` → `dal.MarkUploadSessionCancelled(ctx, tx, ...)`。

- [ ] **Step 6: 改 service/upload.go 的 caller**

```bash
grep -n 'sessionRepo\|dal\.NewUploadSessionRepo' internal/service/upload.go
```

`s.sessionRepo.Xxx(ctx, ...)` → `dal.XxxUploadSession(ctx, s.db, ...)`。

- [ ] **Step 7: 改 service/upload_gc.go 的 caller**

upload_gc.go 里调用 sessionRepo 较多（GC 逻辑）。逐一展开。

- [ ] **Step 8: 改 service/service_test.go**

```bash
grep -n 'sessionRepo\|dal\.NewUploadSessionRepo' internal/service/service_test.go
```

test helper 里 `sessionRepo := dal.NewUploadSessionRepo(db)` 删除。

- [ ] **Step 9: 改 service/upload_gc_test.go**

```bash
grep -n 'sessionRepo\|dal\.NewUploadSessionRepo' internal/service/upload_gc_test.go
```

同上模式。

- [ ] **Step 10: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

- [ ] **Step 11: 提交**

```bash
git add internal/store/dal/upload_session.go internal/store/dal/upload_session_test.go internal/service/
git commit -m "refactor(dal): convert UploadSessionRepo to package-level functions

8 methods converted including TryUploadSessionAdvisoryLock (signature
changed from receiver method to package func taking *gorm.DB). StorageService.
sessionRepo field removed. Tests updated to call package functions directly."
```

---

### Task 9: 提取 filter 类型到 dal/filters.go

**Files:**
- Create: `internal/store/dal/filters.go`
- Modify: `internal/store/dal/file.go`、`audit_log.go`

- [ ] **Step 1: 创建 dal/filters.go**

新文件，内容是把 file.go 和 audit_log.go 里的 filter 类型搬过来：

```go
package dal

import (
    "time"

    storagev1 "storage-service/gen/storage/v1"

    "github.com/servekit/go-common/dbx"
)

// ListFilesFilter defines filtering and pagination options for listing files.
type ListFilesFilter struct {
    PathPrefix        string
    Extension         string
    ContentTypePrefix string
    OrderBy           storagev1.SortField
    Descending        bool
    dbx.Pagination
}

// AdminListFilesFilter defines filtering and pagination options for admin file listing.
// All filter fields are optional — zero values mean "no filter".
type AdminListFilesFilter struct {
    OwnerType         int32
    OwnerID           int64
    PathPrefix        string
    Extension         string
    ContentTypePrefix string
    Vendor            int32 // 0 = no filter
    Bucket            string
    OrderBy           storagev1.SortField
    Descending        bool
    dbx.Pagination
}

// AuditLogFilter defines filtering and pagination options for listing audit logs.
type AuditLogFilter struct {
    OwnerType  int32
    OwnerID    int64
    TargetType int32
    TargetID   int64
    Action     int32
    Status     int32
    RequestID  string
    StartTime  time.Time
    EndTime    time.Time
    dbx.Pagination
}
```

- [ ] **Step 2: 从 file.go 删除 ListFilesFilter 和 AdminListFilesFilter 定义**

打开 `dal/file.go`，找到这两个 type 定义，整段删掉（已经移到 filters.go）。

- [ ] **Step 3: 从 audit_log.go 删除 AuditLogFilter 定义**

打开 `dal/audit_log.go`，找到 AuditLogFilter 定义，整段删掉。

- [ ] **Step 4: build + test**

```bash
go build ./...
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

期望：全绿。filter 类型现在在 `dal` 包内，外部引用 `dal.ListFilesFilter` 等不变。

- [ ] **Step 5: 提交**

```bash
git add internal/store/dal/filters.go internal/store/dal/file.go internal/store/dal/audit_log.go
git commit -m "refactor(dal): extract filter types to filters.go

Cohesion: filters are shared vocabulary across dal files, not owned by
any single table. Centralize per skill's 'single responsibility' guidance."
```

---

## Phase 5: Raw SQL → Typed Raw SQL（TDD，per-table）

每个 task 按 TDD：先写测试，再写模板，再 wire 起来。

### Task 10: StorageObjectQuery 三个聚合模板

**Files:**
- Modify: `internal/store/models/object.go`（扩 StorageObjectQuery interface）
- Regen: `internal/store/generated/`
- Modify: `internal/store/dal/object.go`（3 个聚合方法实现）
- Create: `internal/store/dal/object_query_test.go`

- [ ] **Step 1: 先写测试 dal/object_query_test.go**

新文件 `internal/store/dal/object_query_test.go`：

```go
package dal

import (
    "context"
    "testing"
    "time"

    "github.com/servekit/go-common/dbx"
    "gorm.io/gorm"

    "storage-service/internal/store/models"
)

func setupObjectQueryTestDB(t *testing.T) *gorm.DB {
    t.Helper()
    db := dbx.SetupTestDB(t)
    if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
        t.Fatalf("AutoMigrate: %v", err)
    }
    return db
}

func seedObjects(t *testing.T, db *gorm.DB, objs []models.StorageObject) []int64 {
    t.Helper()
    ids := make([]int64, len(objs))
    for i, o := range objs {
        o.ID = 0 // let DB auto-assign? No — snowflake; here just use autoincrement for test
        if err := db.Create(&o).Error; err != nil {
            t.Fatalf("seed: %v", err)
        }
        ids[i] = o.ID
    }
    return ids
}

func TestCountActiveAndSumObjectSizeByIDs(t *testing.T) {
    db := setupObjectQueryTestDB(t)
    ctx := context.Background()

    ids := seedObjects(t, db, []models.StorageObject{
        {Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "image/png", StorageClass: 1, RefCount: 1},
        {Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "image/png", StorageClass: 1, RefCount: 1},
        {Vendor: 2, Bucket: "b2", ObjectKey: "k3", MD5: "m3", Size: 400, ContentType: "image/png", StorageClass: 1, RefCount: 1},
    })

    // Empty ids short-circuit.
    row, err := CountActiveAndSumObjectSizeByIDs(ctx, db, nil)
    if err != nil || row.TotalObjects != 0 || row.PhysicalBytes != 0 {
        t.Fatalf("empty ids: got row=%+v err=%v", row, err)
    }

    row, err = CountActiveAndSumObjectSizeByIDs(ctx, db, ids)
    if err != nil {
        t.Fatalf("CountActiveAndSumObjectSizeByIDs: %v", err)
    }
    if row.TotalObjects != 3 {
        t.Errorf("TotalObjects: got %d want 3", row.TotalObjects)
    }
    if row.PhysicalBytes != 700 {
        t.Errorf("PhysicalBytes: got %d want 700", row.PhysicalBytes)
    }

    // Soft-deleted rows must be excluded.
    if _, err := DeleteObject(ctx, db, ids[0]); err != nil {
        t.Fatalf("DeleteObject: %v", err)
    }
    row, err = CountActiveAndSumObjectSizeByIDs(ctx, db, ids)
    if err != nil {
        t.Fatalf("after delete: %v", err)
    }
    if row.TotalObjects != 2 || row.PhysicalBytes != 600 {
        t.Errorf("after delete: got row=%+v", row)
    }
}

func TestGroupObjectsByVendorAndSumSize(t *testing.T) {
    db := setupObjectQueryTestDB(t)
    ctx := context.Background()

    ids := seedObjects(t, db, []models.StorageObject{
        {Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "t", StorageClass: 1, RefCount: 1},
        {Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "t", StorageClass: 1, RefCount: 1},
        {Vendor: 2, Bucket: "b2", ObjectKey: "k3", MD5: "m3", Size: 400, ContentType: "t", StorageClass: 1, RefCount: 1},
    })

    rows, err := GroupObjectsByVendorAndSumSize(ctx, db, ids)
    if err != nil {
        t.Fatalf("GroupObjectsByVendorAndSumSize: %v", err)
    }
    if len(rows) != 2 {
        t.Fatalf("want 2 vendor groups, got %d", len(rows))
    }
    byVendor := map[int32]models.ProviderStatRow{}
    for _, r := range rows {
        byVendor[r.Vendor] = r
    }
    if byVendor[1].ObjectCount != 2 || byVendor[1].TotalBytes != 300 {
        t.Errorf("vendor 1: got %+v", byVendor[1])
    }
    if byVendor[2].ObjectCount != 1 || byVendor[2].TotalBytes != 400 {
        t.Errorf("vendor 2: got %+v", byVendor[2])
    }
}

func TestGroupObjectsByBucketAndSumSize(t *testing.T) {
    db := setupObjectQueryTestDB(t)
    ctx := context.Background()

    ids := seedObjects(t, db, []models.StorageObject{
        {Vendor: 1, Bucket: "b1", ObjectKey: "k1", MD5: "m1", Size: 100, ContentType: "t", StorageClass: 1, RefCount: 1},
        {Vendor: 1, Bucket: "b1", ObjectKey: "k2", MD5: "m2", Size: 200, ContentType: "t", StorageClass: 1, RefCount: 1},
        {Vendor: 1, Bucket: "b2", ObjectKey: "k3", MD5: "m3", Size: 400, ContentType: "t", StorageClass: 1, RefCount: 1},
    })

    rows, err := GroupObjectsByBucketAndSumSize(ctx, db, ids)
    if err != nil {
        t.Fatalf("GroupObjectsByBucketAndSumSize: %v", err)
    }
    if len(rows) != 2 {
        t.Fatalf("want 2 bucket groups, got %d", len(rows))
    }
    byBucket := map[string]models.BucketObjectStatRow{}
    for _, r := range rows {
        byBucket[r.Bucket] = r
    }
    if byBucket["b1"].ObjectCount != 2 || byBucket["b1"].TotalBytes != 300 {
        t.Errorf("bucket b1: got %+v", byBucket["b1"])
    }
    if byBucket["b2"].ObjectCount != 1 || byBucket["b2"].TotalBytes != 400 {
        t.Errorf("bucket b2: got %+v", byBucket["b2"])
    }
}

// keep time import used
var _ = time.Now
```

- [ ] **Step 2: 跑测试，确认 FAIL**

```bash
go test -count=1 -run 'TestCountActiveAndSumObjectSizeByIDs|TestGroupObjectsByVendorAndSumSize|TestGroupObjectsByBucketAndSumSize' ./internal/store/dal/
```

期望：FAIL —— 当前实现 `CountActiveAndSumObjectSizeByIDs` 等仍是 Raw SQL 形态，行为可能正确（测试通过）也可能有问题（比如 `seedObjects` 的写法与 snowflake ID 不兼容）。如果测试通过，跳到 Step 4（仍然要把实现改成 Typed Raw SQL，但测试已经"绿"了）；如果失败，按错误调整 seedObjects 或测试逻辑。

**说明**：这一步是 characterization test，目的是先把"当前 Raw SQL 形态"的行为固定下来，确保迁移到 Typed Raw SQL 后行为不变。

- [ ] **Step 3: 在 models/object.go 扩 StorageObjectQuery interface**

打开 `internal/store/models/object.go`，在 `StorageObjectQuery` interface 里加 3 个 SQL-annotated 方法：

```go
type StorageObjectQuery interface {
    // 已有
    // SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
    GetActiveByID(id int64) (StorageObject, error)

    // 新增
    // SELECT COUNT(*) AS total_objects,
    //        COALESCE(SUM(size), 0) AS physical_bytes
    // FROM @@table
    // WHERE id IN (@ids) AND deleted_at IS NULL
    CountActiveAndSumSize(ids []int64) (PhysicalStatsRow, error)

    // 新增
    // SELECT vendor,
    //        COUNT(*) AS object_count,
    //        COALESCE(SUM(size), 0) AS total_bytes
    // FROM @@table
    // WHERE id IN (@ids) AND deleted_at IS NULL
    // GROUP BY vendor
    GroupByVendorCountAndSumSize(ids []int64) ([]ProviderStatRow, error)

    // 新增
    // SELECT bucket,
    //        COUNT(*) AS object_count,
    //        COALESCE(SUM(size), 0) AS total_bytes
    // FROM @@table
    // WHERE id IN (@ids) AND deleted_at IS NULL
    // GROUP BY bucket
    GroupByBucketCountAndSumSize(ids []int64) ([]BucketObjectStatRow, error)
}
```

- [ ] **Step 4: 跑 gorm gen**

```bash
make generate
```

检查 `internal/store/generated/object.gen.go`，确认 `StorageObjectQuery` 类型上多了 `CountActiveAndSumSize` / `GroupByVendorCountAndSumSize` / `GroupByBucketCountAndSumSize` 方法。`@ids` 应该被展开成 `IN ($1, $2, ...)`，`@ids` 在 SQL 里只出现一次但绑定 N 个参数。

- [ ] **Step 5: 改 dal/object.go 里 3 个聚合方法的实现**

把：

```go
func CountActiveAndSumObjectSizeByIDs(ctx context.Context, tx *gorm.DB, ids []int64) (models.PhysicalStatsRow, error) {
    var result models.PhysicalStatsRow
    if len(ids) == 0 {
        return result, nil
    }
    err := tx.WithContext(ctx).
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
```

改成：

```go
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

同样改 `GroupObjectsByVendorAndSumSize`（调 `GroupByVendorCountAndSumSize`）和 `GroupObjectsByBucketAndSumSize`（调 `GroupByBucketCountAndSumSize`）。

同时把方法上方"raw Select required"那段长注释删掉（不再适用）。

- [ ] **Step 6: 跑测试，确认 PASS**

```bash
go test -count=1 -v -run 'TestCountActiveAndSumObjectSizeByIDs|TestGroupObjectsByVendorAndSumSize|TestGroupObjectsByBucketAndSumSize' ./internal/store/dal/
```

期望：3 个测试全 PASS。

- [ ] **Step 7: 跑完整测试套确保无回归**

```bash
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

- [ ] **Step 8: 提交**

```bash
git add internal/store/models/object.go internal/store/generated/ internal/store/dal/object.go internal/store/dal/object_query_test.go
git commit -m "refactor(dal): migrate object aggregations to Typed Raw SQL

Per skill §7, replace db.Model().Select('COUNT(*), SUM(size)').Scan()
with gen-annotated interface methods on StorageObjectQuery:

  CountActiveAndSumSize(ids) → PhysicalStatsRow
  GroupByVendorCountAndSumSize(ids) → []ProviderStatRow
  GroupByBucketCountAndSumSize(ids) → []BucketObjectStatRow

Dal wrappers keep empty-ids short-circuit. Added characterization tests
in dal/object_query_test.go covering empty ids, multi-row, soft-delete
filtering, and per-vendor/bucket grouping."
```

---

### Task 11: StorageFileQuery.FindOwnerObjectIDPairs 模板

**Files:**
- Modify: `internal/store/models/file.go`（扩 StorageFileQuery）
- Regen: `internal/store/generated/`
- Modify: `internal/store/dal/file.go`（FindFileOwnerObjectIDPairs 实现）
- Create: `internal/store/dal/file_query_test.go`

- [ ] **Step 1: 写测试 dal/file_query_test.go**

```go
package dal

import (
    "context"
    "testing"

    "github.com/servekit/go-common/dbx"
    "gorm.io/gorm"

    "storage-service/internal/store/models"
)

func TestFindFileOwnerObjectIDPairs_LimitsAndOrder(t *testing.T) {
    db := setupFileQueryTestDB(t)
    ctx := context.Background()

    // Seed 3 files for owner (1, 100), 1 for (1, 200).
    files := []models.StorageFile{
        {OwnerType: 1, OwnerID: 100, ObjectID: 1001, Filename: "f1"},
        {OwnerType: 1, OwnerID: 100, ObjectID: 1002, Filename: "f2"},
        {OwnerType: 1, OwnerID: 100, ObjectID: 1003, Filename: "f3"},
        {OwnerType: 1, OwnerID: 200, ObjectID: 2001, Filename: "f4"},
    }
    for i := range files {
        if err := db.Create(&files[i]).Error; err != nil {
            t.Fatalf("seed: %v", err)
        }
    }

    pairs, err := FindFileOwnerObjectIDPairs(ctx, db)
    if err != nil {
        t.Fatalf("FindFileOwnerObjectIDPairs: %v", err)
    }
    if len(pairs) != 4 {
        t.Fatalf("want 4 pairs, got %d", len(pairs))
    }

    // Order by id ascending: file[0].id < file[1].id < ...
    // Pairs come back in id order; verify first pair matches first seeded file.
    if pairs[0].OwnerType != 1 || pairs[0].OwnerID != 100 || pairs[0].ObjectID != 1001 {
        t.Errorf("pairs[0]: got %+v", pairs[0])
    }
}

func TestFindFileOwnerObjectIDPairs_SoftDeleteExcluded(t *testing.T) {
    db := setupFileQueryTestDB(t)
    ctx := context.Background()

    f1 := models.StorageFile{OwnerType: 1, OwnerID: 100, ObjectID: 1001, Filename: "f1"}
    f2 := models.StorageFile{OwnerType: 1, OwnerID: 100, ObjectID: 1002, Filename: "f2"}
    if err := db.Create(&f1).Error; err != nil {
        t.Fatalf("seed f1: %v", err)
    }
    if err := db.Create(&f2).Error; err != nil {
        t.Fatalf("seed f2: %v", err)
    }

    // Soft-delete f1.
    if _, err := DeleteFile(ctx, db, f1.ID); err != nil {
        t.Fatalf("DeleteFile: %v", err)
    }

    pairs, err := FindFileOwnerObjectIDPairs(ctx, db)
    if err != nil {
        t.Fatalf("FindFileOwnerObjectIDPairs: %v", err)
    }
    if len(pairs) != 1 {
        t.Fatalf("want 1 pair (soft-deleted excluded), got %d", len(pairs))
    }
    if pairs[0].ObjectID != 1002 {
        t.Errorf("pairs[0]: got %+v", pairs[0])
    }
}

func setupFileQueryTestDB(t *testing.T) *gorm.DB {
    t.Helper()
    db := dbx.SetupTestDB(t)
    if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
        t.Fatalf("AutoMigrate: %v", err)
    }
    return db
}
```

- [ ] **Step 2: 跑测试，观察当前行为**

```bash
go test -count=1 -v -run 'TestFindFileOwnerObjectIDPairs' ./internal/store/dal/
```

期望：当前 `FindFileOwnerObjectIDPairs` 是 Raw SQL 形态，行为应该正确（测试 PASS）。这是 characterization test。

- [ ] **Step 3: 在 models/file.go 扩 StorageFileQuery**

```go
type StorageFileQuery interface {
    // 已有
    // SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
    GetActiveByID(id int64) (StorageFile, error)

    // 已有
    // SELECT object_id, COUNT(*) AS count
    // FROM @@table
    // WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
    // GROUP BY object_id
    GetObjectRefCounts(ownerType int32, ownerID int64) ([]ObjectRefCountRow, error)

    // 已有
    // SELECT COUNT(*) AS count
    // FROM @@table
    // {{where}}
    //   deleted_at IS NULL
    //   {{if ownerType > 0}} AND owner_type = @ownerType {{end}}
    //   {{if ownerID > 0}} AND owner_id = @ownerID {{end}}
    // {{end}}
    GetFileCount(ownerType int32, ownerID int64) (FileCountRow, error)

    // 新增
    // SELECT owner_type, object_id
    // FROM @@table
    // WHERE deleted_at IS NULL
    // ORDER BY id
    // LIMIT @limit
    FindOwnerObjectIDPairs(limit int) ([]OwnerObjectIDPair, error)
}
```

- [ ] **Step 4: 跑 gorm gen**

```bash
make generate
```

- [ ] **Step 5: 改 dal/file.go 的 FindFileOwnerObjectIDPairs 实现**

把：

```go
func FindFileOwnerObjectIDPairs(ctx context.Context, tx *gorm.DB) ([]models.OwnerObjectIDPair, error) {
    var pairs []models.OwnerObjectIDPair
    err := tx.WithContext(ctx).
        Model(&models.StorageFile{}).
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
```

改成：

```go
func FindFileOwnerObjectIDPairs(ctx context.Context, tx *gorm.DB) ([]models.OwnerObjectIDPair, error) {
    pairs, err := generated.StorageFileQuery[models.StorageFile](tx).FindOwnerObjectIDPairs(ctx, MaxObjectIDResults)
    if err != nil {
        return nil, xcodes.ErrInternal.Wrapf(err, "find owner object pairs")
    }
    return pairs, nil
}
```

删掉原方法上方关于 raw Select 的注释（不再适用）。

- [ ] **Step 6: 跑测试 PASS**

```bash
go test -count=1 -v -run 'TestFindFileOwnerObjectIDPairs' ./internal/store/dal/
```

期望：2 个测试全 PASS。

- [ ] **Step 7: 完整测试套无回归**

```bash
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

- [ ] **Step 8: 提交**

```bash
git add internal/store/models/file.go internal/store/generated/ internal/store/dal/file.go internal/store/dal/file_query_test.go
git commit -m "refactor(dal): migrate FindFileOwnerObjectIDPairs to Typed Raw SQL

Adds StorageFileQuery.FindOwnerObjectIDPairs template that selects
(owner_type, object_id) ordered by id with LIMIT. Replaces db.Model().
Select().Scan() in dal wrapper. Tests cover ordering and soft-delete
exclusion."
```

---

### Task 12: StorageQuotaQuery.IncrementUsed + AddQuota 模板

**Files:**
- Modify: `internal/store/models/quota.go`（扩 StorageQuotaQuery）
- Regen: `internal/store/generated/`
- Modify: `internal/store/dal/quota.go`（IncrementQuotaUsed + AddQuota 实现）
- Create: `internal/store/dal/quota_query_test.go`

- [ ] **Step 1: 写测试 dal/quota_query_test.go**

```go
package dal

import (
    "context"
    "testing"

    "github.com/servekit/go-common/dbx"
    "gorm.io/gorm"

    "storage-service/internal/store/models"
    "storage-service/pkg/xcodes"
)

func setupQuotaQueryTestDB(t *testing.T) *gorm.DB {
    t.Helper()
    db := dbx.SetupTestDB(t)
    if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
        t.Fatalf("AutoMigrate: %v", err)
    }
    return db
}

func seedQuota(t *testing.T, db *gorm.DB, ownerType int32, ownerID, total, used int64) *models.StorageQuota {
    t.Helper()
    q := &models.StorageQuota{
        OwnerType:  ownerType,
        OwnerID:    ownerID,
        TotalBytes: total,
        UsedBytes:  used,
    }
    if err := db.Create(q).Error; err != nil {
        t.Fatalf("seed quota: %v", err)
    }
    return q
}

func TestIncrementQuotaUsed_Success(t *testing.T) {
    db := setupQuotaQueryTestDB(t)
    ctx := context.Background()
    seedQuota(t, db, 1, 100, 1000, 100)

    if err := IncrementQuotaUsed(ctx, db, 1, 100, 200); err != nil {
        t.Fatalf("IncrementQuotaUsed: %v", err)
    }
    q, err := GetQuotaByOwner(ctx, db, 1, 100)
    if err != nil {
        t.Fatalf("GetQuotaByOwner: %v", err)
    }
    if q.UsedBytes != 300 {
        t.Errorf("UsedBytes: got %d want 300", q.UsedBytes)
    }
}

func TestIncrementQuotaUsed_ExceedsTotal(t *testing.T) {
    db := setupQuotaQueryTestDB(t)
    ctx := context.Background()
    seedQuota(t, db, 1, 100, 1000, 900)

    err := IncrementQuotaUsed(ctx, db, 1, 100, 200)
    if !xcodes.Is(err, xcodes.ErrQuotaExceeded) {
        t.Fatalf("want ErrQuotaExceeded, got %v", err)
    }

    // Used unchanged.
    q, _ := GetQuotaByOwner(ctx, db, 1, 100)
    if q.UsedBytes != 900 {
        t.Errorf("UsedBytes should be unchanged: got %d want 900", q.UsedBytes)
    }
}

func TestIncrementQuotaUsed_UnknownOwner(t *testing.T) {
    db := setupQuotaQueryTestDB(t)
    ctx := context.Background()

    err := IncrementQuotaUsed(ctx, db, 1, 999, 100)
    if !xcodes.Is(err, xcodes.ErrQuotaExceeded) {
        t.Fatalf("want ErrQuotaExceeded (rowsAffected=0), got %v", err)
    }
}

func TestAddQuota_SuccessAndRefund(t *testing.T) {
    db := setupQuotaQueryTestDB(t)
    ctx := context.Background()
    seedQuota(t, db, 1, 100, 1000, 500)

    if err := AddQuota(ctx, db, 1, 100, 500); err != nil {
        t.Fatalf("AddQuota +500: %v", err)
    }
    q, _ := GetQuotaByOwner(ctx, db, 1, 100)
    if q.TotalBytes != 1500 {
        t.Errorf("after +500: TotalBytes got %d want 1500", q.TotalBytes)
    }

    // Refund 800 (back to 700, still >= 0).
    if err := AddQuota(ctx, db, 1, 100, -800); err != nil {
        t.Fatalf("AddQuota -800: %v", err)
    }
    q, _ = GetQuotaByOwner(ctx, db, 1, 100)
    if q.TotalBytes != 700 {
        t.Errorf("after -800: TotalBytes got %d want 700", q.TotalBytes)
    }
}

func TestAddQuota_RefundTooLarge(t *testing.T) {
    db := setupQuotaQueryTestDB(t)
    ctx := context.Background()
    seedQuota(t, db, 1, 100, 1000, 500)

    // Refund 1500 would push total to -500 → must reject.
    err := AddQuota(ctx, db, 1, 100, -1500)
    if !xcodes.Is(err, xcodes.ErrQuotaInsufficientTotal) {
        t.Fatalf("want ErrQuotaInsufficientTotal, got %v", err)
    }
    q, _ := GetQuotaByOwner(ctx, db, 1, 100)
    if q.TotalBytes != 1000 {
        t.Errorf("TotalBytes should be unchanged: got %d want 1000", q.TotalBytes)
    }
}
```

注意：测试用了 `xcodes.Is(err, xcodes.ErrQuotaExceeded)`，需要确认 `pkg/xcodes` 包是否暴露了 `Is` 函数。如果没有，改用 `errors.Is`（xerr 已实现 Unwrap/Is）：

```go
if !errors.Is(err, xcodes.ErrQuotaExceeded.New()) {
```

实现时按实际 API 调整。

- [ ] **Step 2: 跑测试，确认当前实现 PASS（characterization）**

```bash
go test -count=1 -v -run 'TestIncrementQuotaUsed|TestAddQuota' ./internal/store/dal/
```

期望：PASS。当前 Raw SQL 形态的行为应该正确。

- [ ] **Step 3: 在 models/quota.go 扩 StorageQuotaQuery**

```go
type StorageQuotaQuery interface {
    // 已有
    // SELECT * FROM @@table WHERE id = @id AND deleted_at IS NULL
    GetActiveByID(id int64) (StorageQuota, error)

    // 已有
    // SELECT COALESCE(used_bytes, 0) AS used_bytes
    // FROM @@table
    // WHERE owner_type = @ownerType AND owner_id = @ownerID AND deleted_at IS NULL
    GetUsedBytes(ownerType int32, ownerID int64) (UsedBytesRow, error)

    // 已有
    // SELECT COALESCE(SUM(used_bytes), 0) AS used_bytes
    // FROM @@table
    // WHERE deleted_at IS NULL
    GetTotalUsedBytes() (UsedBytesRow, error)

    // 新增
    // UPDATE @@table
    // {{set}} used_bytes = used_bytes + @bytes {{end}}
    // {{where}}
    //   owner_type = @ownerType
    //   AND owner_id = @ownerID
    //   AND deleted_at IS NULL
    //   AND used_bytes + @bytes <= total_bytes
    // {{end}}
    IncrementUsed(ownerType int32, ownerID int64, bytes int64) (int64, error)

    // 新增
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

- [ ] **Step 4: 跑 gorm gen**

```bash
make generate
```

检查 `internal/store/generated/quota.gen.go`，确认 `StorageQuotaQuery.IncrementUsed` 和 `AddQuota` 生成的方法签名。`@bytes` 在 SQL 模板里被引用两次（SET 和 WHERE），但绑定参数只应有一个 —— 如果 gen 报错或生成了两个参数，需要把模板里的两次引用改成同名占位符（应该已经是了）。

- [ ] **Step 5: 改 dal/quota.go 的 IncrementQuotaUsed 和 AddQuota 实现**

把：

```go
func IncrementQuotaUsed(ctx context.Context, tx *gorm.DB, ownerType int32, ownerID, bytes int64) error {
    rowsAffected, err := gorm.G[models.StorageQuota](tx).
        Where(generated.StorageQuota.OwnerType.Eq(ownerType)).
        Where(generated.StorageQuota.OwnerID.Eq(ownerID)).
        Where(generated.StorageQuota.DeletedAt.IsNull()).
        Where("used_bytes + ? <= total_bytes", bytes).
        Set(generated.StorageQuota.UsedBytes.Incr(bytes)).
        Update(ctx)
    if err != nil {
        return xcodes.ErrInternal.Wrapf(err, "increment used")
    }
    if rowsAffected == 0 {
        return xcodes.ErrQuotaExceeded.New()
    }
    return nil
}
```

改成：

```go
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

`AddQuota` 同模式，`rowsAffected == 0` 映射 `xcodes.ErrQuotaInsufficientTotal.New()`。

删掉原方法上方的"raw SQL cross-column arithmetic required"长注释，改成简短的"通过 Typed Raw SQL 表达跨列算术条件（skill §7）"。

- [ ] **Step 6: 跑测试 PASS**

```bash
go test -count=1 -v -run 'TestIncrementQuotaUsed|TestAddQuota' ./internal/store/dal/
```

期望：5 个测试全 PASS。

- [ ] **Step 7: 完整测试套无回归**

```bash
go test -count=1 ./internal/store/dal/... ./internal/service/...
```

特别留意 `service_test.go` 里的 quota 相关集成测试。

- [ ] **Step 8: 提交**

```bash
git add internal/store/models/quota.go internal/store/generated/ internal/store/dal/quota.go internal/store/dal/quota_query_test.go
git commit -m "refactor(dal): migrate quota arithmetic UPDATEs to Typed Raw SQL

Per skill §7, replace Where(\"used_bytes + ? <= total_bytes\", n) raw
strings with gen-annotated UPDATE templates on StorageQuotaQuery:

  IncrementUsed(ownerType, ownerID, bytes) → rows affected
  AddQuota(ownerType, ownerID, delta) → rows affected

Both use @param referenced twice in template (SET + WHERE); gen binds
once. Dal wrappers preserve rowsAffected==0 → ErrQuotaExceeded /
ErrQuotaInsufficientTotal mapping. Tests cover success, refund, refund-
too-large, unknown owner."
```

---

## Phase 6: 收尾验证

### Task 13: 全量 build / test / lint / drift / 残留扫描

**Files:** 仅运行验证命令，不改代码

- [ ] **Step 1: 全量 build**

```bash
go build ./...
```

期望：无错。

- [ ] **Step 2: 全量测试（含 race）**

```bash
go test -race -count=1 -coverprofile=coverage.out ./...
```

期望：全绿。覆盖率不低于重构前 baseline。

- [ ] **Step 3: golangci-lint**

```bash
make lint
```

期望：无 lint 错。如有 `unused` 报错（比如漏删的 import 或变量），按提示修。

- [ ] **Step 4: generated drift 检查**

```bash
make generate
git diff --exit-code internal/store/generated
```

期望：无 diff（提交的 generated 与最新 models 一致）。

- [ ] **Step 5: 残留扫描 —— 不应再有 raw Select().Scan()**

```bash
grep -RnE '\.Select\(' internal/store/dal
```

期望：空输出（所有聚合都已迁移到模板）。

- [ ] **Step 6: 残留扫描 —— 不应再有 raw Where("...")**

```bash
grep -RnE 'Where\("[^"]*[\?\+]' internal/store/dal
```

期望：空输出（跨列算术都已迁移）。

- [ ] **Step 7: 残留扫描 —— 不应再有 Repo struct**

```bash
grep -RnE 'type [A-Z][a-zA-Z]*Repo struct' internal/store/dal
grep -RnE 'func New[A-Z][a-zA-Z]*Repo' internal/store/dal
```

期望：空输出。

- [ ] **Step 8: 残留扫描 —— StorageService struct 不应再有 repo 字段**

```bash
grep -nE '[a-zA-Z]+Repo\s+\*dal\.' internal/service/service.go
```

期望：空输出。

- [ ] **Step 9: 残留扫描 —— StorageUploadSession 不应有 TableName()**

```bash
grep -Rn 'func (StorageUploadSession) TableName' internal/store/models
```

期望：空输出。

- [ ] **Step 10: 残留扫描 —— service 层不应再有 dal.NewXxxRepo**

```bash
grep -RnE 'dal\.New[A-Z][a-zA-Z]*Repo' internal/service
```

期望：空输出。

- [ ] **Step 11: 残留扫描 —— import 不应再有 repository**

```bash
grep -Rn 'internal/store/repository' internal/ cmd/ pkg/ 2>/dev/null
```

期望：空输出。

- [ ] **Step 12: 提交 coverage.out（可选）**

如果项目有把 coverage.out 提交到 CI 比较的习惯，提交。否则 `.gitignore`。

- [ ] **Step 13: 推送分支并开 PR**

```bash
git push -u origin feat/audit-logging
gh pr create --title "refactor(store): align internal/store with gorm-cli-development skill" --body "$(cat <<'EOF'
## Summary

- Rename repository/ → dal/, package-level function style per skill §6
- Prefix all model structs with Storage (StorageFile, StorageQuota, ...)
- Drop Storage service-prefix from dal method names per skill §6 example
- Rename gen-annotated interfaces (FileQuery → StorageFileQuery, ...)
- Migrate 6 Raw SQL aggregations / cross-column arithmetic UPDATEs to
  Typed Raw SQL templates per skill §7
- StorageService struct simplified to hold only db (drop 5 repo fields)
- DBRecorder holds db instead of AuditLogRepo

## Spec

docs/superpowers/specs/2026-06-18-internal-store-dal-refactor-design.md

## Test plan

- [x] go build ./...
- [x] go test -race ./...
- [x] golangci-lint run ./...
- [x] make generate && git diff --exit-code internal/store/generated
- [x] No remaining raw Select().Scan() in dal
- [x] No remaining raw Where(\"...\") in dal
- [x] No remaining XxxRepo struct in dal
- [x] No remaining repo fields in StorageService
- [x] No remaining import of internal/store/repository

## Notes

DB schema breaking change: tables files/quotas/audit_logs/upload_sessions
→ storage_files/storage_quotas/storage_audit_logs/storage_upload_sessions.
Project still in development; AutoMigrate creates the new tables, old
empty ones to be dropped manually.
EOF
)"
```

注意：`gh pr create` 之前先与用户确认是否真的要开 PR（这是个有副作用的操作）。

---

## Self-Review 检查

完成所有 task 后回头自查：

- [ ] spec §1 七条目标都有对应 task？— §1.1→Task 1, §1.2→Task 4-8, §1.3→Task 1, §1.4→Task 4-8 命名, §1.5→Task 2, §1.6→Task 4-8 service struct, §1.7→Task 10-12
- [ ] spec §2.1 所有"做"项都覆盖？— 目录改名(Task 3), struct 改名(Task 1), AllModels(Task 1), dal 包级函数(Task 4-8), filters.go(Task 9), constants.go(unchanged 但 Task 3 改 package), test 文件(Task 8), service 13 文件(Task 4-8 + Task 1), StorageService 简化(Task 4-8), DBRecorder(Task 7), generated regen(Task 1/2/10/11/12)
- [ ] spec §2.2 "不做"项都没被计划？— schema migration 没做 ✓, 辅助行 struct 没改 ✓, Raw SQL 都迁移了（与 §1.7 一致）✓, 命令行/migrate 不动 ✓, builder 查询没迁移 ✓
- [ ] spec §3 表名变更？— Task 1 通过 struct rename + 移除 TableName() 实现，AutoMigrate 自动建新表
- [ ] spec §4 方法命名规则？— Task 4-8 按映射表执行
- [ ] spec §5.1 完整方法名映射表？— Task 4-8 一一对应
- [ ] spec §5.3 Raw SQL 迁移？— Task 10/11/12 完整覆盖
- [ ] spec §6.2 interface 改名 + 新方法？— Task 2 改名, Task 10/11/12 加新方法
- [ ] spec §7 service 改动？— Task 4-8 全部覆盖
- [ ] spec §9 验证命令？— Task 13 全部跑一遍
- [ ] spec §9.1 新测试计划？— Task 10 (object_query_test.go), Task 11 (file_query_test.go), Task 12 (quota_query_test.go)
- [ ] spec §10 风险？— Task 10-12 通过 TDD + characterization test 缓解风险 4；gen 双引用参数在 Task 12 Step 4 显式验证

无 placeholder。类型一致性：spec 用 `dal.CreateStorageFile` 还是 `dal.CreateFile`？— **spec §4 明确用 `dal.CreateFile`（去掉 Storage 前缀）**，plan Task 5 也用 `dal.CreateFile`。一致。

---

## Phase 7: 实施时发现的额外清理（post-spec）

### Task 14: 移除冗余 `Where(DeletedAt.IsNull())` 过滤

**Status:** DONE (commit `356d3722`)

**背景**：实施完成后用户 review 发现 —— GORM 的软删 auto-filter 已经会自动给所有 builder 查询加 `WHERE deleted_at IS NULL`，dal/ 里 21 处手动 `Where(generated.Xxx.DeletedAt.IsNull())` 是冗余的。

**清理范围**：
- `dal/object.go` (6 处)
- `dal/file.go` (7 处)
- `dal/quota.go` (5 处)
- `dal/upload_session.go` (3 处)

**保留**：
- `dal/object.go` 的 `FindPurgeableObjects` / `PurgeObject`（用 `Unscoped()` + `DeletedAt.IsNotNull()` / `DeletedAt.Lt()`，必须手动）
- `models/*.go` 里 Typed Raw SQL 模板的 `deleted_at IS NULL`（模板走 `e.Raw()` 绕过 auto-filter）
- 结构体 tag 里的 `condition:deleted_at IS NULL`（Postgres 部分索引定义，不是查询过滤）

**实施时遇到的小坑**：`gorm.G[T](tx)` 返回 `Interface[T]`，需要 `.Where()` 或 `.Scopes()` 才能进 `ChainInterface[T]` 以便后续条件赋值。`ListAllFiles`（file.go）和 `FindObjectIDsByFilter`（object.go）这两个函数原本用 `DeletedAt.IsNull()` 当类型桥梁，删掉后用 `.Scopes(func(*gorm.Statement) {})` noop 桥接替代。

**验证**：现有 characterization tests（`TestCountActiveAndSumObjectSizeByIDs_SoftDeleteExcluded`、`TestFindFileOwnerObjectIDPairs_SoftDeleteExcluded`）仍通过，证明 auto-filter 行为等价。

### Task 12 实际产出（spec 偏差）

**Status:** DONE-WITH-EXCEPTION (commit `aa4e98d`)

spec §1 item 7 原计划 6 处 Raw SQL 全部迁移到 Typed Raw SQL。实际迁了 4 处，2 处保留：

- ✅ `StorageObjectQuery.CountActiveAndSumSize` / `GroupByVendorCountAndSumSize` / `GroupByBucketCountAndSumSize`（3 个聚合）
- ✅ `StorageFileQuery.FindOwnerObjectIDPairs`（1 个投影）
- ⚠️ `dal.IncrementQuotaUsed`、`dal.AddQuota` —— 保留 Raw SQL `Where("col + ? op col", n)`，原因：gorm.io/cli v0.2.4 UPDATE 模板 codegen 无法返回 `RowsAffected`（详见 spec §5.4 和 dal/quota.go 的 doc comment）

5 个 characterization tests（`dal/quota_query_test.go`）锁定保留方法的行为。
