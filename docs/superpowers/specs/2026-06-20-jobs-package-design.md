# jobs 调度包设计 — 把 upload session GC 从 upload 包中独立出来

**日期**：2026-06-20
**状态**：已批准，待实施
**关联**：
- `docs/superpowers/specs/2026-06-20-domain-subpackage-extraction-design.md`（全域子包化重构）
- `docs/superpowers/specs/2026-06-16-upload-session-design.md`（upload session 与 GC 设计）

## 背景

Phase 4 子包化重构完成后，upload session 的 GC 逻辑挂在 `upload.Service` 上：

- `internal/service/upload/gc.go` 提供 `Service.RunOnce(ctx)` —— 扫描过期 PENDING session、回收 OSS 孤儿对象
- `internal/service/upload/upload.go` 提供 `Service.RegisterGC()` —— 把 RunOnce 注册到 cron
- `upload.Service` 持有 `cron *cron.Cron` 字段，由 `upload.Deps.Cron` 注入
- `internal/service/service.go` 在 `New()` 里创建 cron 实例（`resolveCron`），传给 upload，再调 `upload.RegisterGC()`
- `StorageService.RunOnce(ctx)` 作为 facade 暴露给外部
- cron 实例通过 `cronComponent`（适配 `lifecycle.Service`）挂到 service 的 lifecycle.Manager

### 痛点

1. **职责错位**：GC 不是 upload 领域的一个 RPC，它是「对 upload session 资源的定期维护操作」。把它绑在 `upload.Service` 上，让 upload 包同时承担「同步 RPC handler」和「后台调度入口」两个角色。
2. **命名暗示错误的契约**：`RunOnce` 这个名字暗示「被外部触发一次」，但实际上只有 cron closure 和测试在调用，没有 admin RPC、没有外部触发入口。`StorageService.RunOnce` 这层 facade 也是为对称而存在，没有真实消费者。
3. **扩展点散落**：未来要加 file 软删除清理、quota 重算、临时文件清理等定时任务时，每个领域都要在自己包里写 `RegisterXxx` + 持有 cron，调度配置散落在多个包，cron 实例归属混乱。
4. **upload 包依赖膨胀**：`upload.Service` 实际只用到 GC 的 4 个依赖（db/registry/cfg/host），但因为持有 cron，多了 1 个无关依赖。

## 设计目标

- **职责归位**：业务逻辑（reap expired sessions）挂在领域 service 上；调度逻辑（cron 注册、生命周期）集中在独立的 jobs 包。
- **包按领域划分的约定不被破坏**：file/upload/audit/quota/sts/admin 都是领域包，jobs 是「调度层」，与 service 平级（类比 middleware、provider）。
- **cron 归 jobs 所有**：jobs 自包含 cron 实例 + lifecycle，service 不再碰调度。
- **扩展点集中**：新增定时任务只需在 `jobs.Scheduler.register()` 里加一行 AddFunc，并按需扩展 `Deps`。
- **不破坏调用方**：`Server.Start()` / `NewModule` 的入口签名保持不变（仅内部装配调整）。

## 包结构变动

### 新建：`internal/jobs/`

```
internal/jobs/
└── jobs.go    # Scheduler + New + Start + Stop + WithCron option + register
```

与 `internal/service/`、`internal/store/`、`internal/provider/`、`internal/thirdcall/` 平级。理由：jobs 是「调度/编排层」，不是领域子模块。把它塞进 `internal/service/` 下会把「做什么」（领域）和「什么时候做」（调度）两个维度混在一起。

### 依赖方向

```
internal/service  ──>  internal/jobs  ──>  internal/service/upload
       │                                              ▲
       └──────────────────────────────────────────────┘
```

- `service.New()` 创建 `*jobs.Scheduler`，挂到 service 的 lifecycle.Manager
- `jobs.New()` 接收 `*upload.Service` 作为 Deps，调用 `upload.ReapExpiredSessions`
- `service` 包与 `service/upload` 子包是两个不同的 Go 包，**不构成 import 循环**

### upload 包内变动

| 项 | 改动 |
|----|------|
| `gc.go` | 重命名为 `reap.go`（文件名跟随方法名） |
| `gc_test.go` | 重命名为 `reap_test.go`；测试函数 `TestRunOnce_*` → `TestReapExpiredSessions_*` |
| `Service.RunOnce(ctx)` | 重命名为 `Service.ReapExpiredSessions(ctx)` |
| `gcAdvisoryLockKey` 常量 | 重命名为 `reapAdvisoryLockKey`（**值 `0x534F4C47` 不变**，DB 测试钉住） |
| `Service.RegisterGC()` 方法 | **删除**（注册逻辑移到 jobs） |
| `Service.cron` 字段 | **删除**（upload 不再持有 cron） |
| `Deps.Cron` 字段 | **删除** |

### service 包变动

| 项 | 改动 |
|----|------|
| `StorageService.cron` 字段 | **删除** |
| `cronComponent` 类型（helpers.go） | **删除**（被 jobs.Scheduler 替代） |
| `resolveCron` 函数（helpers.go） | **删除**（cron 创建移到 jobs.New） |
| service.New 里 `cronInst := resolveCron(...)` | **删除** |
| service.New 里 `svc.upload.RegisterGC()` 调用 | **删除** |
| `StorageService.RunOnce(ctx)` facade | **删除**（无消费者） |
| `StorageService.Upload()` getter | **新增**（让外部能拿到底层 upload.Service 喂给 jobs.New） |
| `StorageService.AddExtra(name, lifecycle.Service)` | **新增**（让外部把 jobs.Scheduler 挂到 service.mgr） |

### pkg/option 变动

| 项 | 改动 |
|----|------|
| `option.WithCron(c *cron.Cron)` | **删除**（转移到 jobs） |
| `Options.Cron` 字段 | **删除** |

当前无任何调用方使用 `WithCron`（已 grep 确认 `_test.go` 和外部入口均未引用），删除是安全的。

## jobs.Scheduler API

```go
package jobs

import (
    "context"
    "fmt"
    "log/slog"
    "time"

    "storage-service/internal/service/upload"
    "storage-service/pkg/config"

    "github.com/servekit/go-common/cronx"
    "github.com/servekit/go-common/lifecycle"
    "github.com/robfig/cron/v3"
)

// Scheduler schedules periodic background jobs. It owns the cron scheduler —
// callers add it to a lifecycle.Manager; Start launches the scheduler, Stop
// blocks until in-flight jobs drain.
//
// To add a new job: append an AddFunc inside register, and if it needs a new
// domain service, extend Deps.
type Scheduler struct {
    cron   *cron.Cron
    cfg    *config.Config
    upload *upload.Service
}

// Deps injects the domain services that jobs will schedule.
type Deps struct {
    Cfg    *config.Config
    Upload *upload.Service
}

type Option func(*options)
type options struct { injectedCron *cron.Cron }

// WithCron injects an existing cron.Cron (typically for tests). If unset,
// jobs.New builds one from cfg.Storage.Cron and owns its lifecycle.
func WithCron(c *cron.Cron) Option {
    return func(o *options) { o.injectedCron = c }
}

// Compile-time assertion that *Scheduler satisfies lifecycle.Service.
var _ lifecycle.Service = (*Scheduler)(nil)

// New builds the cron scheduler, registers all jobs, and returns a Scheduler
// ready to be added to a lifecycle.Manager.
func New(d *Deps, opts ...Option) (*Scheduler, error) {
    var o options
    for _, opt := range opts { opt(&o) }

    c := o.injectedCron
    if c == nil {
        timezone := d.Cfg.Storage.Cron.Timezone
        if timezone == "" {
            timezone = "Asia/Shanghai"
        }
        var err error
        c, err = cronx.New(&cronx.Config{Timezone: timezone, OverlapPolicy: "skip"})
        if err != nil {
            return nil, fmt.Errorf("jobs: init cron: %w", err)
        }
    }

    s := &Scheduler{cron: c, cfg: d.Cfg, upload: d.Upload}
    if err := s.register(); err != nil {
        return nil, err
    }
    return s, nil
}

// register wires all periodic jobs to the scheduler. Future jobs (file
// soft-delete sweep, quota recompute, etc.) are added here.
func (s *Scheduler) register() error {
    spec := s.cfg.Storage.UploadGC.CronSpec
    if spec == "" {
        spec = "*/5 * * * *"
    }
    if _, err := s.cron.AddFunc(spec, func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        if _, err := s.upload.ReapExpiredSessions(ctx); err != nil {
            slog.Error("jobs: reap expired upload sessions", "error", err)
        }
    }); err != nil {
        return fmt.Errorf("jobs: register upload reap: %w", err)
    }
    return nil
}

// Start launches the cron scheduler. The scheduler runs jobs in its own
// goroutines, so Start returns immediately.
func (s *Scheduler) Start() error { s.cron.Start(); return nil }

// Stop signals the scheduler to halt and blocks until all in-flight jobs
// finish. Under lifecycle.Manager this runs concurrently with other Stoppers
// (db/redis/gid close); job failures during shutdown are logged and abandoned.
func (s *Scheduler) Stop() error { <-s.cron.Stop().Done(); return nil }
```

## service.StorageService 新增的 public 方法

```go
// Upload returns the upload sub-service, so callers (pkg.Server, pkg.NewModule)
// can wire it into jobs.New.
func (s *StorageService) Upload() *upload.Service { return s.upload }

// AddExtra attaches an externally-built lifecycle.Service (e.g. jobs.Scheduler)
// to this service's lifecycle.Manager. Start/Stop will include it.
func (s *StorageService) AddExtra(name string, c lifecycle.Service) {
    s.manager.Add(name, c)
}
```

`Upload()` 是为了暴露内部子服务给 jobs.New；`AddExtra()` 是为了让外部构建的 jobs 能挂到 service 已有的 lifecycle.Manager，复用统一的 Start/Stop 调度，关闭顺序与 db/redis/gid 一致。

## server.go / module.go 装配

### pkg/server.go

```go
svc, err := service.New(cfg, o.serviceOpts...)
if err != nil { return nil, err }

scheduler, err := jobs.New(&jobs.Deps{Cfg: cfg, Upload: svc.Upload()})
if err != nil {
    return nil, errors.Join(fmt.Errorf("init jobs: %w", err), svc.Stop())
}
svc.AddExtra("jobs", scheduler)

hdl := handler.FromService(svc)
// 后续 grpcSrv 装配不变。
// Server.Start() 通过 hdl.Start → svc.Start → mgr.Start 自动启动 jobs。
```

### pkg/module.go（NewModule）

当前实现是 `handler.New(cfg, opts...)` 一行。改为显式装配 service + jobs + handler：

```go
func NewModule(cfg *config.Config, opts ...option.Option) (*handler.Handler, error) {
    svc, err := service.New(cfg, opts...)
    if err != nil { return nil, err }

    scheduler, err := jobs.New(&jobs.Deps{Cfg: cfg, Upload: svc.Upload()})
    if err != nil { return errors.Join(err, svc.Stop()) }
    svc.AddExtra("jobs", scheduler)

    return handler.FromService(svc), nil
}
```

`handler.New` 保留（仍可单独使用），但 `NewModule` 改走显式装配以包含 jobs。

## upload.ReapExpiredSessions 的实现

把当前 `internal/service/upload/gc.go` 的 `RunOnce` 方法体原样搬到 `reap.go` 并改名为 `ReapExpiredSessions`，内部逻辑完全不变：

- 获取 advisory lock（`gcAdvisoryLockKey` → `reapAdvisoryLockKey`）
- 列出过期 PENDING session（`dal.ListExpiredPendingUploadSessions`）
- 对每个 session：HeadObject 判断 orphan，CAS 标记 EXPIRED，DeleteObject 回收
- 通过 `s.host.RecordOutcome` 记录审计事件

依赖（最小集）：`db`、`registry`、`cfg`、`host`。不再需要 `cron` 字段。

## 不改动项

- `cfg.Storage.UploadGC` 配置项（YAML 兼容；jobs 包消费即可）
- `cfg.Storage.Cron.Timezone`（jobs.New 读取）
- `dal.TryUploadSessionAdvisoryLock` / `ListExpiredPendingUploadSessions` / `MarkUploadSessionExpired` 等函数名（本来就准确，叫 UploadSession）
- `AuditAction_AUDIT_ACTION_UPLOAD_SESSION_GC` proto 枚举（向前兼容）
- advisory lock 常量值 `0x534F4C47`（DB 测试钉住，仅重命名常量名）
- GC 内部的 CAS / HA replica 防重 / transient error 重试等逻辑（已是稳定的修复成果）

## 测试变动

### upload 包

- `upload/gc_test.go` → `upload/reap_test.go`
- 测试函数 `TestRunOnce_OrphanCleanup` → `TestReapExpiredSessions_OrphanCleanup`（其余 4 个测试同理改名）
- 测试 setup 不再向 `upload.New` 传 Cron
- 测试内部 `svc.RunOnce(ctx)` → `svc.ReapExpiredSessions(ctx)`

### service 包

- `service_test.go:918`：`svc.RunOnce(ctx)` → `svc.upload.ReapExpiredSessions(ctx)`（test 直接调领域 service，不再走已删除的 facade）

### jobs 包（新增，结构性测试）

`*upload.Service` 是具体类型不是接口，fake 成本高，所以 jobs 包只做结构性测试（不实际触发 ReapExpiredSessions）：

- `internal/jobs/jobs_test.go` 验证：
  - `New` 不传 WithCron 时成功创建 Service，且内部 cron 非 nil（默认自建路径）
  - `New` 传 WithCron(fake) 时，Service 持有的是注入实例（不创建新 cron）—— 通过 `svc.cron` 字段比对（暴露一个 test-only getter 或 package-internal 字段访问）
  - `register()` 后 `cron.Entries()` 长度 ≥ 1（任务已挂上）
  - `Start()` 立即返回（不阻塞），`Stop()` 在无 in-flight job 时立即返回
  - 无效 spec（如 `"invalid-cron"`）让 `New` 返回 error

构造 `*upload.Service` 实例可以用最小依赖（`db=sqlmock 不实际触发`、`registry=空`、`cfg=默认`、`host=nil-impl`），因为测试不会走到 ReapExpiredSessions 的实际逻辑。

## 实施顺序（建议）

1. 在 `internal/service/upload/` 内完成重命名：`gc.go` → `reap.go`，`RunOnce` → `ReapExpiredSessions`，`gcAdvisoryLockKey` → `reapAdvisoryLockKey`，更新 `reap_test.go` 和 `service_test.go` 的调用方。**编译通过、测试全绿。**
2. 新建 `internal/jobs/jobs.go`，实现 Service/New/Start/Stop/WithCron/register。jobs 此时调 `upload.ReapExpiredSessions`。
3. 修改 `internal/service/`：删除 `cronComponent`、`resolveCron`、`StorageService.cron` 字段、service.New 里的 cron 装配、`StorageService.RunOnce` facade、`upload.RegisterGC` 方法和 `upload.Deps.Cron` 字段；新增 `StorageService.Upload()` 和 `StorageService.AddExtra()`。
4. 修改 `pkg/option/option.go`：删除 `WithCron` 和 `Options.Cron`。
5. 修改 `pkg/server.go` 和 `pkg/module.go`：加入 jobs.New + svc.AddExtra("jobs", jobsSvc) 装配。
6. 跑全套测试 + `golangci-lint run ./...` + `go build ./...` 确认。

每步独立可编译，便于回滚。

## 未来扩展点（不实现，仅说明）

- **file 软删除清理**：`file.Service.SweepSoftDeleted(ctx)` + 在 `jobs.Scheduler.register()` 加一行 AddFunc + 扩展 `Deps.File`。
- **quota 重算**：`quota.Service.RecomputeUsage(ctx)` + 同上。
- **STS cache 失效扫描**：`sts.Service.SweepStaleEntries(ctx)` + 同上。

这些场景出现时，无需新建包，只需领域 service 加方法 + jobs.register 加一行。

## 实施后调整（2026-06-20）

实施完成后的回顾暴露出两个问题：

1. **jobs 包的 `Option/WithCron` 机制多余**：除了内部测试，没有任何外部消费者。`WithCron` 看起来像是为了对称才存在，实际可由 `Deps.Cron` 字段直接表达。
2. **`pkg/server.go` 和 `pkg/module.go` 重复装配 jobs**：两处各写一份 `jobs.New + svc.AddExtra` 模板代码，且为了支撑它被迫给 `StorageService` 加了 `Upload()` 和 `AddExtra()` 两个 public 方法（仅 jobs 装配用）。

**最终设计（已实施）：**

- `internal/jobs/jobs.go`：`Scheduler` 变成纯调度器。删 `upload` 字段、`Deps.Upload` 字段、`register()` 方法、`Option/options/WithCron` 机制。`Deps` 改为 `{Cfg, Cron}`（Cron 可选注入，nil 则自建）。新增 `AddFunc(spec, cmd) error` 暴露 cron 注册能力。**不再 import `internal/service/upload`**。
- `internal/service/service.go`：在 `New()` 内创建 `jobs.Scheduler`，加入 `mgr`，然后用 `scheduler.AddFunc(spec, fn)` 注册 upload reap 任务。`StorageService.Upload()` 和 `StorageService.AddExtra()` **删除**——它们不再需要。
- `pkg/server.go`、`pkg/module.go`：**回滚**到 jobs 抽取前的形态（`hdl := handler.FromService(svc)` / `handler.New(cfg, opts...)`）。

依赖方向：`service → jobs`（jobs 不 import service 子包，无循环），更干净。

**收益：**
- jobs 包代码量减少 ~40%（无 Option 机制、无 register、无 upload import）
- `StorageService` public API 缩小（少 2 个方法）
- `pkg/server.go` 和 `pkg/module.go` 回到一行装配
- 未来加新任务：在 `service.New` 加一个 `scheduler.AddFunc` 调用即可（不在 jobs 包内）

**代价：**
- service 包 import jobs 包（之前是 jobs 包 import upload 子包）——只是依赖方向调换，无循环。

## 关联

- 实施 plan：`docs/superpowers/plans/2026-06-20-jobs-package.md`
- 上游 spec：[[2026-06-20-domain-subpackage-extraction-design]]、[[2026-06-16-upload-session-design]]、[[2026-06-16-lifecycle-integration-design]]
