# storage-service 全域子包化重构 — 设计

**日期**：2026-06-20
**状态**：已批准，待实施
**关联**：`docs/superpowers/specs/2026-06-20-storage-service-skill-refactor-design.md`（原始 4-Phase 重构 spec）

## 背景

原始重构 spec 的 Phase 3 已经把 upload 域升级为 `internal/service/upload/` 子包，并在 service.go 写了 6 个 facade 方法。但 audit/file/quota/admin 四个域的方法仍直接挂在 `*StorageService` 上，散落在各自的文件里。

用户反馈："service 必须提供一个完整的门脸类，散落在各个地方很不方便，service.go 里要和 handler 一一对应"。

为了实现"service.go 是 handler 的 1-to-1 镜像"这一目标，把剩余 4 个域也升级为子包，service.go 收纳全部 27 个 facade 方法。

## 与原始 spec 的关系

本 spec 是原始 spec 的**补充**。原 Phase 4（HTTP 注解 + grpcx.New 三件套）顺延为：
- **Phase 5**：HTTP annotation 设计 + 实施
- **Phase 6**：grpcx.New 五参数补全

原始 Phase 1-3 已实施完毕。本 spec 是新增的 **Phase 4**（替代原 Phase 4 的位置）。

## 现状（截至 Phase 3 完成）

| 域 | 当前位置 | 方法数 | 当前状态 |
|----|---------|--------|---------|
| upload | `internal/service/upload/` 子包 | 6 RPC + RunOnce | ✅ 已子包化，service.go 有 facade |
| audit | `internal/service/audit.go` | 2 RPC + 基础设施 | 直接在 `*StorageService` 上 |
| file | `internal/service/file.go` | 7 RPC + GetMyQuota | 直接在 `*StorageService` 上 |
| quota | `internal/service/quota.go` | 2 RPC + getQuota/setQuota/addQuota helper | 直接在 `*StorageService` 上 |
| admin | `internal/service/admin.go` + `stats.go` + `cleanup.go` | 10 RPC + getStorageStats + Purge* | 直接在 `*StorageService` 上 |

总计：21 个 RPC 方法 + 跨域 helper，散落在 6 个文件。

## 跨域依赖分析

经 grep 调研，各域间调用关系：

```
file.go    → audit.recordOutcome, quota.getQuota, helpers.buildUserFileInfo, helpers.protoToImageOp
quota.go   → audit.recordOutcome
admin.go   → audit.recordOutcome, quota.{getQuota,setQuota}, helpers.{buildAdminFileInfo, vendorToName, aclStringToProto}, stats.getStorageStats
upload/*   → audit (via Host.RecordOutcome), quota (via Host.CheckQuota/Reserve), helpers (本地副本)
```

**关键发现**：
- **audit**（Recorder、Event、snapshot types、recordOutcome）是**基础设施**，被所有域使用
- **quota** 的 `getQuota/setQuota/addQuota` 是**基础设施**，被 file/admin/upload 使用
- **helpers.go** 的 `buildUserFileInfo/buildAdminFileInfo/ownerTypeToProto/vendorToName/aclStringToProto/resolveBucket/objectKeyFromMD5/protoToImageOp` 是**纯函数工具集**

## 设计

### 4 个新子包

#### `internal/service/audit/`

包含 audit 域所有内容（基础设施性质）：
- `Event` 结构体
- `Recorder` interface
- `FileSnapshot / QuotaSnapshot / OwnerDeletionResult / FileBatchDeleteResult / UploadSessionSnapshot` 类型（从 audit.go 迁入）
- `DBRecorder` 实现 + `NewDBRecorder(db, gid)` 构造函数（从 audit.go 迁入）
- `recordOutcome / recordOutcomeInTx` 方法（从 audit.go 迁入，挂在 `*Service` 上）
- `ListMyAuditLogs / AdminListAuditLogs` RPC 实现

**Exported**：`Service`, `New(db, gid)`, `Recorder`（interface，给其它包注入用）, `Event`, 各 snapshot 类型

**Host 接口**：无（audit 是叶子域，不回调父包）

#### `internal/service/quota/`

包含 quota 域：
- `getQuota / setQuota / addQuota` helper（从 quota.go 迁入）
- `SetOwnerQuota / AddOwnerQuota` RPC（从 quota.go 迁入）
- `GetMyQuota` RPC（**从 file.go 迁入**，语义属 quota）

**Exported**：`Service`, `New(db)`, `GetQuota/SetQuota/AddQuota`（公开给其它包调用）

**Host 接口**：无

**Audit 注入**：quota.Service 需要 audit.Recorder 来记录配额变更审计。`Deps` 字段 `Audit audit.Recorder`，由父包注入。

#### `internal/service/file/`

包含 file 域：
- `GenerateDownloadURL / ListMyFiles / GetMyFile / UpdateMyFile / DeleteMyFile / BatchDeleteMyFiles / GenerateProcessURL` RPC（从 file.go 迁入）
- `buildUserFileInfo` helper（从 helpers.go 迁入，file 域专属）

**Exported**：`Service`, `New(Deps)`

**Host 接口**：
```go
type Host interface {
    CheckQuota(ctx, db, ownerType, ownerID, requiredBytes) error
    Reserve(ctx, tx, ownerType, ownerID, bytes) error
    Release(ctx, tx, ownerType, ownerID, bytes) error  // for DeleteMyFile
    RecordOutcome(ctx, event audit.Event, err error)
}
```

**Deps**：DB, GID, Registry, Cfg, Audit (audit.Recorder), Quota (quota.Service), Host

#### `internal/service/admin/`

包含 admin 域 + stats + cleanup：
- 10 个 Admin* RPC（从 admin.go 迁入）
- `getStorageStats` helper（从 stats.go 迁入）
- `PurgeDeletedObjects / PurgeDeletedOwner / DeletedOwnerRetention` helper（从 cleanup.go 迁入）
- `buildAdminFileInfo` helper（从 helpers.go 迁入）

**Exported**：`Service`, `New(Deps)`

**Host 接口**：同 file 域 + 任何 admin 特有需求

**Deps**：DB, GID, Registry, Cfg, Audit, Quota, Host

### Shared helpers

纯函数 utility（`ownerTypeToProto / vendorToName / aclStringToProto / resolveBucket / objectKeyFromMD5 / protoToImageOp`）—— **保留在父包 `internal/service/`** 的 `helpers.go` 里，因为：
1. 它们是无状态纯函数
2. 多个子包都要用
3. 子包 import 父包会循环；让父包暴露这些 utility 函数（导出为 `PascalCase`），子包通过 Deps 注入或 Host 接口暴露

**决策**：父包定义 `Converter` interface 装这些 utility，子包通过 Deps 注入：

```go
// internal/service/converters.go (新文件)
type Converters struct {
    OwnerTypeToProto func(int32) storagev1.OwnerType
    VendorToName     func(int32) string
    ACLToProto       func(string) storagev1.BucketACL
    // ...
}
```

或者更简单：这些纯函数本来就是 `internal/service/helpers.go` 的 free function，让子包直接 import 一个新的 `internal/service/conv/` 子包：

```
internal/service/conv/
└── conv.go   # ownerTypeToProto, vendorToName, etc. (all exported)
```

各子包 `import "storage-service/internal/service/conv"` 即可，无循环依赖。

**最终方案**：建 `internal/service/conv/` 子包，纯函数 utility 全部迁入并 export。

### service.go facade

`StorageService` struct 持有 5 个子包实例：

```go
type StorageService struct {
    cfg      *config.Config
    manager  *lifecycle.Manager
    audit    *audit.Service
    quota    *quota.Service
    upload   *upload.Service
    file     *file.Service
    admin    *admin.Service
}
```

service.go 有 **27 个 facade 方法**，与 handler/storage.go 的 27 个委托方法**一一对应**：

```go
// Upload
func (s *StorageService) GenerateUploadURL(...) { return s.upload.GenerateUploadURL(...) }
func (s *StorageService) ConfirmUpload(...)     { return s.upload.ConfirmUpload(...) }
// ... 6 total

// File
func (s *StorageService) GenerateDownloadURL(...) { return s.file.GenerateDownloadURL(...) }
// ... 7 total

// Quota
func (s *StorageService) GetMyQuota(...)      { return s.quota.GetMyQuota(...) }
func (s *StorageService) SetOwnerQuota(...)   { return s.quota.SetOwnerQuota(...) }
func (s *StorageService) AddOwnerQuota(...)   { return s.quota.AddOwnerQuota(...) }

// Admin
func (s *StorageService) AdminListFiles(...) { return s.admin.AdminListFiles(...) }
// ... 10 total

// Audit
func (s *StorageService) ListMyAuditLogs(...)     { return s.audit.ListMyAuditLogs(...) }
func (s *StorageService) AdminListAuditLogs(...)  { return s.audit.AdminListAuditLogs(...) }
```

`pkg/handler/storage.go` 不变（依然调 `svc.X`），因为 facade 方法名与之前直接挂在 `*StorageService` 上的方法名一致。

### New() 装配顺序

```go
func New(cfg, opts...) (*StorageService, error) {
    o := option.Apply(opts...)
    mgr := lifecycle.NewManager()

    db, _ := resolveDB(cfg, o.DB, mgr)
    gid, _ := resolveGID(cfg, o.GIDService, mgr)
    redisClient, _ := resolveRedis(cfg, o.Redis, mgr)
    registry, _ := provider.NewRegistry(cfg.Storage.Providers)
    cronInst, _ := resolveCron(cfg, o.Cron, mgr)

    // Audit first — quota/file/admin depend on it
    auditSvc := audit.New(audit.Deps{DB: db, GID: gid})

    // Quota next — file/admin depend on it
    quotaSvc := quota.New(quota.Deps{DB: db, Audit: auditSvc.Recorder()})

    svc := &StorageService{
        cfg: cfg, manager: mgr,
        audit: auditSvc, quota: quotaSvc,
        // ... others set below
    }

    // Upload — uses Host for quota + audit callbacks
    svc.upload = upload.New(upload.Deps{
        DB: db, Registry: registry, GID: gid, Cfg: cfg,
        Limiter: limiter, Redis: redisClient,
        STSCfg: ..., DedupLock: ..., Host: svc, Cron: cronInst,
    })
    svc.upload.RegisterGC()

    // File — uses Host for quota release + audit
    svc.file = file.New(file.Deps{
        DB: db, Registry: registry, GID: gid, Cfg: cfg,
        Audit: auditSvc.Recorder(), Quota: quotaSvc, Host: svc,
    })

    // Admin — uses Host + quota + audit
    svc.admin = admin.New(admin.Deps{
        DB: db, Registry: registry, GID: gid, Cfg: cfg,
        Audit: auditSvc.Recorder(), Quota: quotaSvc, Host: svc,
    })

    return svc, nil
}
```

### Host 接口的复用

`upload.Host` 已经有 `CheckQuota/Reserve/RecordOutcome`。file/admin 需要类似能力 + `Release`（释放配额）+ `ConvertOwnerType`（utility）。

**方案**：把 Host 提升到 `internal/service/host.go`，作为父包提供的统一接口。各子包 import 这个接口：

```go
// internal/service/host.go
type Host interface {
    CheckQuota(ctx, db, ownerType, ownerID, requiredBytes) error
    Reserve(ctx, tx, ownerType, ownerID, bytes) error
    Release(ctx, tx, ownerType, ownerID, bytes) error
    RecordOutcome(ctx, audit.Event, err)
}
```

但这样会让子包 import 父包（循环！）。所以 Host 必须定义在子包内（每个子包自己的 Host interface），父包实现所有 Host interface。

**最终方案**：每个子包定义自己的 Host interface（最小集），父包 `*StorageService` 通过 `var _ upload.Host = (*StorageService)(nil)` 等编译期断言保证实现。upload 已经这么做了；file/admin/quota 沿用。

### 测试迁移

每个子包有自己的 `*_test.go`：
- `audit/audit_test.go`（含原 service_test.go 中 audit 相关测试）
- `quota/quota_test.go`
- `file/file_test.go`
- `admin/admin_test.go`

`internal/service/service_test.go` 只保留**集成测试**（涉及多域协作的端到端流程，如完整 upload+confirm+delete 流程）。

## PR 节奏

为了控制风险，分 4 个独立 commit（每个新子包一个）：

1. `refactor(service): extract audit subpackage` — audit 域子包化 + service.go 加 2 个 facade
2. `refactor(service): extract quota subpackage` — quota 域子包化 + service.go 加 3 个 facade（含 GetMyQuota 迁入）
3. `refactor(service): extract file subpackage` — file 域子包化 + service.go 加 7 个 facade
4. `refactor(service): extract admin subpackage + finalize 27-RPC facade` — admin 域子包化 + service.go 加 10 个 facade + 清理 stats.go/cleanup.go + 删除跨域 helper

加 1 个 setup commit：
0. `refactor(service): extract conv subpackage for shared converters` — utility 函数先抽出

每个 commit 后 `make test`（非 Docker 部分）必须通过。

## 非目标

- 不改业务逻辑
- 不改 proto 定义
- 不改 handler（handler 调 `svc.X` 不变，因为 facade 方法名与原方法名一致）
- 不改 store/dal/models
- 不改 upload/ 子包（已成型）
- 不动 lifecycle.Manager 资源管理（Phase 2 完成）

## 风险

1. **Host interface 爆炸**：4 个子包各自 Host，父包要实现 4 套接口。如果接口有重叠（如 RecordOutcome），父包方法重复定义会冲突。**对策**：让 audit.Recorder 接口成为标准，所有子包通过 Deps 注入 audit.Recorder，不通过 Host。Host 只放 quota 相关 + 域特定回调。

2. **Deps 注入字段膨胀**：每个子包 Deps 字段多。**对策**：用 functional options 模式或在 Deps 内嵌结构体。先观察实际字段数，必要时重构。

3. **测试覆盖断层**：迁移测试时可能漏。**对策**：每个 commit 后跑全量 `go test ./...`，对比迁移前后的测试函数清单（`grep -c "^func Test" internal/service/*_test.go` 前后对比）。

4. **循环依赖**：子包互相 import 风险。**对策**：依赖方向严格单向（audit ← quota ← {file, admin}；upload 独立），任何反向 import 立即 build 失败暴露。

## 验收

完成后：
- [ ] `go build ./...` 通过
- [ ] `grep -c "^func (s \*StorageService)" internal/service/service.go` 应为 **27**（全部 facade 在 service.go）
- [ ] `internal/service/` 目录只剩：service.go / helpers.go（resolveDB 等 + conv 入口）/ host.go（如有）/ cron_component.go / 各子包入口
- [ ] 原始 `audit.go / file.go / quota.go / admin.go / stats.go / cleanup.go / helpers.go`（除 resolve 函数外）内容全部移走到子包
- [ ] pkg/handler/storage.go 不变（27 个委托方法名不变）
- [ ] `go test -race ./...` 通过（非 Docker 部分）

## 关联

- **前置 spec**：`docs/superpowers/specs/2026-06-20-storage-service-skill-refactor-design.md`
- **后续 spec**（HTTP annotation）：待 Phase 4 完成后撰写
- **Skill 参考**：`ai-kit-studio/skills/golang-service-development/golang-service-development.md` §1, §2
- **upload 子包参考实现**：`internal/service/upload/`
