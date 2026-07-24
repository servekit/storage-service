# Jobs Package Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 upload session GC 从 `upload.Service` 抽取到独立的 `internal/jobs` 包，让 cron 完全归 jobs 所有，service 不再碰调度。

**Architecture:** 新建 `internal/jobs/` 包（与 service 平级），提供 `Scheduler` 类型（持有 cron + 实现 `lifecycle.Service`）。`upload.Service` 保留业务方法 `ReapExpiredSessions(ctx)`，删除其 cron 相关字段/方法。`server.go` / `module.go` 入口装配 jobs 并通过 `svc.AddExtra("jobs", scheduler)` 挂到 service 的 lifecycle.Manager。

**Tech Stack:** Go 1.22+，`github.com/servekit/go-common/{cronx,lifecycle}`，`github.com/robfig/cron/v3`，PostgreSQL testcontainer，miniredis，testify。

**Spec:** `docs/superpowers/specs/2026-06-20-jobs-package-design.md`

---

## File Structure

| 路径 | 操作 | 责任 |
|------|------|------|
| `internal/service/upload/gc.go` | 重命名 → `reap.go`；方法/常量重命名 | 业务逻辑：reap expired sessions |
| `internal/service/upload/gc_test.go` | 重命名 → `reap_test.go`；测试函数重命名 | upload 包 reap 测试 |
| `internal/service/upload/setup_test.go` | 修改：去掉 `Cron` 字段 | upload 测试 setup |
| `internal/service/upload/upload.go` | 修改：删 `cron` 字段、`Deps.Cron` 字段、`RegisterGC` 方法 | upload Service 定义 |
| `internal/jobs/jobs.go` | 新建 | `Scheduler` + `New` + `Start/Stop` + `WithCron` + `register` |
| `internal/jobs/jobs_test.go` | 新建 | 结构性测试 |
| `internal/service/helpers.go` | 修改：删 `cronComponent` + `resolveCron`；可能保留其他 helper | service 辅助 |
| `internal/service/service.go` | 修改：删 cron 装配/facade；新增 `Upload()` + `AddExtra()` | StorageService |
| `internal/service/service_test.go` | 修改：`svc.RunOnce` → `svc.upload.ReapExpiredSessions` | service 集成测试 |
| `pkg/option/option.go` | 修改：删 `WithCron` + `Options.Cron` | DI option |
| `pkg/server.go` | 修改：装配 jobs | gRPC server entry |
| `pkg/module.go` | 修改：装配 jobs | in-process module entry |

---

## Task 1: upload 包内 GC 重命名（gc → reap）

**目标：** 把 `gc.go` 重命名为 `reap.go`，方法 `RunOnce` → `ReapExpiredSessions`，常量 `gcAdvisoryLockKey` → `reapAdvisoryLockKey`，测试函数同步改名。**纯重命名，不改逻辑。** 完成后编译通过、所有现有测试通过。

**Files:**
- Rename: `internal/service/upload/gc.go` → `internal/service/upload/reap.go`
- Rename: `internal/service/upload/gc_test.go` → `internal/service/upload/reap_test.go`
- Modify: `internal/service/upload/setup_test.go`
- Modify: `internal/service/service_test.go`

- [ ] **Step 1: 用 git mv 重命名 gc.go → reap.go**

```bash
git mv internal/service/upload/gc.go internal/service/upload/reap.go
```

- [ ] **Step 2: 用 git mv 重命名 gc_test.go → reap_test.go**

```bash
git mv internal/service/upload/gc_test.go internal/service/upload/reap_test.go
```

- [ ] **Step 3: 在 reap.go 内重命名方法 RunOnce → ReapExpiredSessions**

修改 `internal/service/upload/reap.go`：

把方法签名
```go
// RunOnce scans one batch of expired PENDING sessions and cleans up OSS orphans.
// Pure logic — caller (cron, admin RPC, test) decides when to invoke.
// Returns the count of orphan objects deleted from cloud storage.
//
// HA safety: acquires a session-level advisory lock before scanning so that
// two replicas running GC cron concurrently do not both process the same
// batch. If the lock cannot be acquired (another replica holds it), returns
// (0, nil) without touching OSS.
func (s *Service) RunOnce(ctx context.Context) (int, error) {
```

改为：
```go
// ReapExpiredSessions scans one batch of expired PENDING sessions and cleans
// up OSS orphans. Pure logic — caller (jobs.Scheduler, test) decides when to
// invoke. Returns the count of orphan objects deleted from cloud storage.
//
// HA safety: acquires a session-level advisory lock before scanning so that
// two replicas running GC cron concurrently do not both process the same
// batch. If the lock cannot be acquired (another replica holds it), returns
// (0, nil) without touching OSS.
func (s *Service) ReapExpiredSessions(ctx context.Context) (int, error) {
```

同时把 reap.go 内部的注释里 `RunOnce` 改为 `ReapExpiredSessions`（line 103 附近：`Reclaiming those is out of RunOnce's current scope` → `out of ReapExpiredSessions's current scope`）。

- [ ] **Step 4: 在 reap.go 内重命名常量 gcAdvisoryLockKey → reapAdvisoryLockKey**

修改 `internal/service/upload/reap.go`：

```go
// gcAdvisoryLockKey is the PostgreSQL advisory-lock key identifying the
// upload-session GC lease. Two HA replicas running GC cron simultaneously
// will contend on this single key; only one acquires and proceeds per cycle.
// The value 0x534F4C47 ("SOLG") is arbitrary but stable; tests pin the same
// value (see internal/store/dal/upload_session_test.go).
const gcAdvisoryLockKey int64 = 0x534F4C47
```

改为：
```go
// reapAdvisoryLockKey is the PostgreSQL advisory-lock key identifying the
// upload-session reap lease. Two HA replicas running the reap job
// simultaneously will contend on this single key; only one acquires and
// proceeds per cycle. The value 0x534F4C47 ("SOLG") is arbitrary but
// stable; tests pin the same value (see internal/store/dal/upload_session_test.go).
const reapAdvisoryLockKey int64 = 0x534F4C47
```

注意：**值 `0x534F4C47` 不变**（DB 测试钉住）。

把 reap.go 内对该常量的引用 `gcAdvisoryLockKey` 改为 `reapAdvisoryLockKey`（在 `dal.TryUploadSessionAdvisoryLock(ctx, s.db, gcAdvisoryLockKey)` 这一行）。

- [ ] **Step 5: 在 reap_test.go 内重命名测试函数 + 调用**

修改 `internal/service/upload/reap_test.go`：

测试函数改名（5 个）：
- `TestRunOnce_OrphanCleanup` → `TestReapExpiredSessions_OrphanCleanup`
- `TestRunOnce_NoUploadSkipsDelete` → `TestReapExpiredSessions_NoUploadSkipsDelete`
- `TestRunOnce_ConfirmedSessionNotDeletedRaceFix` → `TestReapExpiredSessions_ConfirmedSessionNotDeletedRaceFix`
- `TestRunOnce_TransientErrorRetries` → `TestReapExpiredSessions_TransientErrorRetries`
- `TestRunOnce_HAReplicasDoNotDoubleProcess` → `TestReapExpiredSessions_HAReplicasDoNotDoubleProcess`

测试注释里的 `RunOnce` 改为 `ReapExpiredSessions`（共 7 处，包括函数文档注释和行内注释如 `BEFORE RunOnce reaches its CAS`）。

测试体内调用 `svc.RunOnce(ctx)` 改为 `svc.ReapExpiredSessions(ctx)`（共 7 处：line 46, 80, 125, 169, 183, 234, 238）。

- [ ] **Step 6: 修改 setup_test.go，去掉 Cron 字段（暂时保留 Deps.Cron 字段不删）**

修改 `internal/service/upload/setup_test.go` 的 `setupUploadServiceWithFakeProvider`：

把
```go
	svc := New(&Deps{
		DB:        db,
		Registry:  registry,
		GID:       gid,
		Cfg:       cfg,
		Redis:     rdb,
		STS:       &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour},
		DedupLock: NewDedupLock(rdb, &config.LockConfig{}),
		Host:      host,
		Cron:      cron.New(),
	})
```

改为（删 `Cron: cron.New(),` 一行）：
```go
	svc := New(&Deps{
		DB:        db,
		Registry:  registry,
		GID:       gid,
		Cfg:       cfg,
		Redis:     rdb,
		STS:       &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour},
		DedupLock: NewDedupLock(rdb, &config.LockConfig{}),
		Host:      host,
	})
```

同时删除文件顶部不再使用的 `"github.com/robfig/cron/v3"` import（如果 goimports 没自动处理，手工删 line 17）。

- [ ] **Step 7: 修改 service_test.go:918，svc.RunOnce → svc.upload.ReapExpiredSessions**

修改 `internal/service/service_test.go` 的 `TestUpload_GCFlow`（line 884 起）：

把
```go
	// 5. Run GC.
	deleted, err := svc.RunOnce(ctx)
```

改为：
```go
	// 5. Run reap.
	deleted, err := svc.upload.ReapExpiredSessions(ctx)
```

- [ ] **Step 8: 编译并跑 upload 包测试，确认通过**

Run:
```bash
go build ./...
go test -race ./internal/service/upload/...
```
Expected: 编译通过，所有 reap 测试 PASS（5 个测试函数，新名字）。

- [ ] **Step 9: 跑 service 包测试，确认 TestUpload_GCFlow 通过**

Run:
```bash
go test -race -run TestUpload_GCFlow ./internal/service/...
```
Expected: PASS。

- [ ] **Step 10: Commit**

```bash
git add internal/service/upload/reap.go internal/service/upload/reap_test.go \
        internal/service/upload/setup_test.go internal/service/service_test.go
git commit -m "$(cat <<'EOF'
refactor(upload): rename GC to reap — RunOnce → ReapExpiredSessions

Pure rename. gc.go → reap.go, gcAdvisoryLockKey → reapAdvisoryLockKey
(value 0x534F4C47 unchanged), test functions TestRunOnce_* →
TestReapExpiredSessions_*. Setup drops Cron field (still accepted by
Deps for now; removed in a later task). No behavior change.
EOF
)"
```

---

## Task 2: 新建 internal/jobs 包

**目标：** 创建 `internal/jobs/jobs.go` 提供 `Scheduler` 类型（持有 cron + 实现 `lifecycle.Service`），调 `upload.Service.ReapExpiredSessions`。配套结构性测试。完成后编译通过、jobs 包测试通过。**这一步不修改任何现有包，纯新增。**

**Files:**
- Create: `internal/jobs/jobs.go`
- Create: `internal/jobs/jobs_test.go`

- [ ] **Step 1: 创建 internal/jobs/jobs.go**

新建文件 `internal/jobs/jobs.go`：

```go
// Package jobs owns the cron scheduler for storage-service periodic
// background work. The Scheduler type implements lifecycle.Service so it
// integrates with the parent service's lifecycle.Manager — Start launches
// the scheduler, Stop blocks until in-flight jobs drain.
//
// To add a new periodic job: append an AddFunc inside register, and if it
// needs a new domain service, extend Deps.
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

// Scheduler owns the cron instance and registers periodic jobs against it.
// Callers add it to a lifecycle.Manager; Start launches the scheduler, Stop
// blocks until in-flight jobs drain.
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

// Option configures a Scheduler instance.
type Option func(*options)

type options struct {
	injectedCron *cron.Cron
}

// WithCron injects an existing cron.Cron (typically for tests). If unset,
// New builds one from cfg.Storage.Cron and owns its lifecycle.
func WithCron(c *cron.Cron) Option {
	return func(o *options) { o.injectedCron = c }
}

// Compile-time assertion that *Scheduler satisfies lifecycle.Service.
var _ lifecycle.Service = (*Scheduler)(nil)

// New builds the cron scheduler (or reuses the injected one), registers all
// periodic jobs, and returns a Scheduler ready to be added to a
// lifecycle.Manager.
func New(d *Deps, opts ...Option) (*Scheduler, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

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
// soft-delete sweep, quota recompute, etc.) are added here as additional
// AddFunc calls.
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
func (s *Scheduler) Start() error {
	s.cron.Start()
	return nil
}

// Stop signals the scheduler to halt and blocks until all in-flight jobs
// finish. Under lifecycle.Manager this runs concurrently with other
// Stoppers (db/redis/gid close); job failures during shutdown are logged
// and abandoned — sessions get reaped next cycle.
func (s *Scheduler) Stop() error {
	<-s.cron.Stop().Done()
	return nil
}
```

- [ ] **Step 2: 创建 internal/jobs/jobs_test.go（结构性测试）**

新建文件 `internal/jobs/jobs_test.go`：

```go
package jobs

import (
	"testing"

	"storage-service/pkg/config"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalCfg returns a Config with just enough fields for jobs.New to read
// without panicking. Domain services in Deps are nil because the structural
// tests below never trigger ReapExpiredSessions.
func minimalCfg() *config.Config {
	return &config.Config{
		Storage: &config.StorageConfig{},
	}
}

// TestNew_DefaultBuildsCron verifies that when WithCron is NOT passed, New
// builds its own cron instance (non-nil) and registers the reap job.
func TestNew_DefaultBuildsCron(t *testing.T) {
	s, err := New(&Deps{Cfg: minimalCfg(), Upload: nil})
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.NotNil(t, s.cron, "Scheduler should build its own cron when none injected")
	assert.Len(t, s.cron.Entries(), 1, "reap job should be registered")
}

// TestNew_WithCronUsesInjected verifies that an injected cron is used
// as-is (no new cron created).
func TestNew_WithCronUsesInjected(t *testing.T) {
	injected := cron.New()
	s, err := New(&Deps{Cfg: minimalCfg(), Upload: nil}, WithCron(injected))
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Same(t, injected, s.cron, "Scheduler should use the injected cron instance")
	assert.Len(t, injected.Entries(), 1, "reap job should be registered on the injected cron")
}

// TestNew_InvalidSpecReturnsError verifies that a malformed CronSpec makes
// New fail with a non-nil error.
func TestNew_InvalidSpecReturnsError(t *testing.T) {
	cfg := minimalCfg()
	cfg.Storage.UploadGC = &config.UploadGCConfig{CronSpec: "not-a-valid-cron-spec"}
	_, err := New(&Deps{Cfg: cfg, Upload: nil})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "register upload reap")
}

// TestStartStop_NonBlocking verifies Start returns immediately and Stop
// returns promptly when no jobs are in-flight.
func TestStartStop_NonBlocking(t *testing.T) {
	s, err := New(&Deps{Cfg: minimalCfg(), Upload: nil})
	require.NoError(t, err)

	require.NoError(t, s.Start())
	require.NoError(t, s.Stop())
}
```

注意：`config.UploadGCConfig` 字段名要按实际 config 包定义。实施时若名字不同（比如 `UploadGCConfig` vs `UploadGC`），按 `pkg/config` 包里的实际类型名为准。

- [ ] **Step 3: 跑 jobs 包测试，确认通过**

Run:
```bash
go test -race -v ./internal/jobs/...
```
Expected: 4 个测试全部 PASS。

如果 `config.UploadGCConfig` 字段名或 `UploadGC.CronSpec` 路径与实际不符，编译会报错——按 `pkg/config/config.go` 里的真实定义修正 jobs.go 和 jobs_test.go。

- [ ] **Step 4: 跑 go vet + goimports 检查**

Run:
```bash
go vet ./internal/jobs/...
goimports -w internal/jobs/jobs.go internal/jobs/jobs_test.go
gofmt -w internal/jobs/jobs.go internal/jobs/jobs_test.go
```
Expected: 无 vet 报错，文件格式化通过。

- [ ] **Step 5: Commit**

```bash
git add internal/jobs/jobs.go internal/jobs/jobs_test.go
git commit -m "$(cat <<'EOF'
feat(jobs): add internal/jobs package owning the cron scheduler

Scheduler implements lifecycle.Service (Start/Stop) and registers
upload.ReapExpiredSessions as a periodic job. WithCron option supports
test injection; otherwise the scheduler builds its own cron via cronx.
service/server/module wiring follows in subsequent commits.
EOF
)"
```

---

## Task 3: service 包停止使用 cron

**目标：** 让 `internal/service` 不再创建 cron、不再调 `upload.RegisterGC`、不再持有 `StorageService.cron` 字段、不再有 `RunOnce` facade。`upload.Service.cron/RegisterGC/Deps.Cron` 暂时保留为 dead code（下一 task 清理）。完成后编译通过、所有测试通过。

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/helpers.go`

- [ ] **Step 1: 在 service.go 删除 cronInst 装配代码**

修改 `internal/service/service.go`。

把 `New()` 函数里：
```go
	cronInst, err := resolveCron(cfg, o.Cron, mgr)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init cron: %w", err), mgr.Stop())
	}
```
**整段删除**。

- [ ] **Step 2: 在 service.go 删除 StorageService 构造里的 cron 字段赋值**

把 `svc := &StorageService{ ... }` 里的 `cron: cronInst,` 一行删除。

- [ ] **Step 3: 在 service.go 删除 StorageService.cron 字段**

把 `StorageService` struct 里的 `cron *cron.Cron` 字段删除。

- [ ] **Step 4: 在 service.go 删除 svc.upload.RegisterGC() 调用**

把 `New()` 函数里：
```go
	// RegisterGC adds the GC job to the cron scheduler. Must happen before
	// mgr.Start (which starts cronComponent → c.Start).
	if err := svc.upload.RegisterGC(); err != nil {
		return nil, errors.Join(fmt.Errorf("register upload gc: %w", err), mgr.Stop())
	}
```
**整段删除**。

- [ ] **Step 5: 在 service.go 删除 StorageService.RunOnce facade**

把：
```go
// RunOnce delegates the upload-session GC pass to the subpackage.
func (s *StorageService) RunOnce(ctx context.Context) (int, error) {
	return s.upload.RunOnce(ctx)
}
```
**整段删除**。

注意：`upload.Service.RunOnce` 已经在 Task 1 改名为 `ReapExpiredSessions`，所以这里 facade 内的 `s.upload.RunOnce` 早已编译失败——如果 Task 1 之后没立即跟 Task 3，这一步是修复编译失败的关键。本 plan 假定连续执行。

- [ ] **Step 6: 在 service.go 删除不再使用的 cron import**

把 `"github.com/robfig/cron/v3"` import 删除（line 24 附近）。

注意：如果 `helpers.go` 仍引用 cron（cronComponent 还在），service.go 的 cron import 删除后**整个包还能编译**，因为 import 是包级别的。但如果 service.go 完全不直接用 cron，goimports 会警告——可以保留也可以删，**建议删**让依赖清晰。

- [ ] **Step 7: 编译，确认通过**

Run:
```bash
go build ./...
```
Expected: 编译通过。`helpers.go` 的 cronComponent/resolveCron 仍在（dead code），不报错。

- [ ] **Step 8: 跑全套测试，确认通过**

Run:
```bash
go test -race ./...
```
Expected: 所有测试 PASS。`service_test.go:918` 已在 Task 1 改为 `svc.upload.ReapExpiredSessions`，所以 facade 删除不破坏测试。

- [ ] **Step 9: Commit**

```bash
git add internal/service/service.go
git commit -m "$(cat <<'EOF'
refactor(service): drop cron ownership from StorageService

StorageService no longer creates a cron, no longer calls
upload.RegisterGC, and loses its cron field + RunOnce facade. The
adapters (cronComponent, resolveCron) become dead code — removed in
the next commit when upload-side cron fields are also dropped.
EOF
)"
```

---

## Task 4: 清理 upload 包 dead code + 新增 service.Upload()/AddExtra()

**目标：** 删除 `upload.Service.cron` 字段、`upload.Deps.Cron` 字段、`upload.Service.RegisterGC` 方法、`helpers.go` 的 `cronComponent`/`resolveCron`；新增 `StorageService.Upload()` getter 和 `StorageService.AddExtra(name, lifecycle.Service)` 方法。完成后 service 包和 upload 包都没有 cron 残留，新方法可用。

**Files:**
- Modify: `internal/service/upload/upload.go`
- Modify: `internal/service/helpers.go`
- Modify: `internal/service/service.go`

- [ ] **Step 1: 在 upload.go 删除 upload.Service.cron 字段**

修改 `internal/service/upload/upload.go` 的 `Service` struct：

把字段块
```go
	sts       *sts.Service
	dedupLock DedupLocker
	host      Host
	cron      *cron.Cron
}
```
改为
```go
	sts       *sts.Service
	dedupLock DedupLocker
	host      Host
}
```

- [ ] **Step 2: 在 upload.go 删除 upload.Deps.Cron 字段**

修改 `Deps` struct：

把字段块
```go
	DedupLock DedupLocker
	Host      Host
	Cron      *cron.Cron
}
```
改为
```go
	DedupLock DedupLocker
	Host      Host
}
```

- [ ] **Step 3: 在 upload.go 删除 upload.Service.RegisterGC 方法**

把整个方法删除：
```go
// RegisterGC schedules the periodic upload-session GC. Call once during parent
// service construction (after upload.New). The cron instance is shared with the
// parent — upload.Service does not own it.
func (s *Service) RegisterGC() error {
	spec := s.cfg.Storage.UploadGC.CronSpec
	if spec == "" {
		spec = "*/5 * * * *"
	}
	_, err := s.cron.AddFunc(spec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := s.RunOnce(ctx); err != nil {
			slog.Error("upload gc run", "error", err)
		}
	})
	return err
}
```

- [ ] **Step 4: 在 upload.go 删除 New() 里 cron 字段赋值**

修改 `New()` 函数：

把
```go
	return &Service{
		db:        d.DB,
		registry:  d.Registry,
		gid:       d.GID,
		cfg:       d.Cfg,
		limiter:   d.Limiter,
		sts:       sts.New(d.Redis, issuer, d.STS),
		dedupLock: d.DedupLock,
		host:      d.Host,
		cron:      d.Cron,
	}
```
改为（删 `cron: d.Cron,` 一行）：
```go
	return &Service{
		db:        d.DB,
		registry:  d.Registry,
		gid:       d.GID,
		cfg:       d.Cfg,
		limiter:   d.Limiter,
		sts:       sts.New(d.Redis, issuer, d.STS),
		dedupLock: d.DedupLock,
		host:      d.Host,
	}
```

- [ ] **Step 5: 在 upload.go 删除不再使用的 import**

把 `"github.com/robfig/cron/v3"` 和（如果不再使用）`"log/slog"`、`"time"`、`"context"` 删除。`context` 仍被其他方法使用，保留；`time` 也常被其他方法用（token 过期等），保留；`slog` 在其他 RPC 路径也常用，保留。**只删 `cron` import**。

注意：实施时跑 `goimports -w internal/service/upload/upload.go` 自动清理。

- [ ] **Step 6: 在 helpers.go 删除 cronComponent 类型 + resolveCron 函数**

修改 `internal/service/helpers.go`。

删除整个 `cronComponent` 类型（含编译期断言、Start、Stop 方法、注释，约 30 行）：
```go
// cronComponent adapts *cron.Cron to lifecycle.Service. ...
type cronComponent struct {
	c *cron.Cron
}

var _ lifecycle.Service = (*cronComponent)(nil)

func (cc *cronComponent) Start() error {
	cc.c.Start()
	return nil
}

func (cc *cronComponent) Stop() error {
	<-cc.c.Stop().Done()
	return nil
}
```

删除整个 `resolveCron` 函数：
```go
func resolveCron(cfg *config.Config, injected *cron.Cron, mgr *lifecycle.Manager) (*cron.Cron, error) {
	if injected != nil {
		return injected, nil
	}
	timezone := cfg.Storage.Cron.Timezone
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	c, err := cronx.New(&cronx.Config{Timezone: timezone, OverlapPolicy: "skip"})
	if err != nil {
		return nil, err
	}
	mgr.Add("cron", &cronComponent{c: c})
	return c, nil
}
```

同时清理因此不再使用的 import：`"github.com/servekit/go-common/cronx"` 和 `"github.com/robfig/cron/v3"`。`lifecycle` 通常其他 helper 也用（resolveDB/resolveRedis 等），保留。

- [ ] **Step 7: 在 service.go 新增 StorageService.Upload() getter**

修改 `internal/service/service.go`，在合适位置（建议在 `New()` 之后、其他委托方法之前，或在文件末尾的 helpers 区）添加：

```go
// Upload returns the upload sub-service, so callers (pkg.Server, pkg.NewModule)
// can wire it into jobs.New.
func (s *StorageService) Upload() *upload.Service { return s.upload }
```

- [ ] **Step 8: 在 service.go 新增 StorageService.AddExtra() 方法**

紧接 `Upload()` 之后添加：

```go
// AddExtra attaches an externally-built lifecycle.Service (e.g. jobs.Scheduler)
// to this service's lifecycle.Manager. Start/Stop will include it.
func (s *StorageService) AddExtra(name string, c lifecycle.Service) {
	s.manager.Add(name, c)
}
```

- [ ] **Step 9: 编译，确认通过**

Run:
```bash
go build ./...
```
Expected: 编译通过。

- [ ] **Step 10: 跑全套测试，确认通过**

Run:
```bash
go test -race ./...
```
Expected: 所有测试 PASS。`upload/setup_test.go` 已在 Task 1 去掉 `Cron: cron.New()`，所以 `Deps.Cron` 字段删除不破坏测试。

- [ ] **Step 11: Commit**

```bash
git add internal/service/upload/upload.go internal/service/helpers.go \
        internal/service/service.go
git commit -m "$(cat <<'EOF'
refactor: purge upload/service cron dead code, add Upload()+AddExtra()

upload.Service loses its cron field, Deps.Cron field, and RegisterGC
method (replaced by internal/jobs). helpers.go loses cronComponent and
resolveCron. StorageService gains Upload() (for jobs.New wiring) and
AddExtra() (for attaching jobs.Scheduler to its lifecycle.Manager).
EOF
)"
```

---

## Task 5: 删除 pkg/option.WithCron + server/module 装配 jobs

**目标：** 删除 `pkg/option.WithCron` 和 `Options.Cron` 字段；修改 `pkg/server.go` 和 `pkg/module.go`，在创建 service 后构造 `jobs.Scheduler` 并通过 `svc.AddExtra("jobs", ...)` 挂到 lifecycle。

**Files:**
- Modify: `pkg/option/option.go`
- Modify: `pkg/server.go`
- Modify: `pkg/module.go`

- [ ] **Step 1: 在 pkg/option/option.go 删除 WithCron + Options.Cron**

修改 `pkg/option/option.go`：

把 `Options` struct 里的 `Cron *cron.Cron` 字段删除：
```go
type Options struct {
	DB         *gorm.DB
	Redis      *redis.Client
	GIDService thirdcall.GIDService
	Cron       *cron.Cron  // ← 删这一行
}
```

把 `WithCron` 函数整段删除：
```go
// WithCron provides an existing cron.Cron instance.
// If not set, the service creates one from config.Storage.Cron and owns its
// lifecycle (Stop on shutdown). When injected, the caller manages lifecycle.
func WithCron(c *cron.Cron) Option {
	return func(o *Options) { o.Cron = c }
}
```

把不再使用的 `"github.com/robfig/cron/v3"` import 删除。

- [ ] **Step 2: 修改 pkg/server.go，装配 jobs**

修改 `pkg/server.go`：

在 import 块加 `"storage-service/internal/jobs"`（按字母序插入 `internal/service` 之后）。

在 `NewServer` 函数里，`svc, err := service.New(...)` 之后、`hdl := handler.FromService(svc)` 之前，加入 jobs 装配：

把
```go
	svc, err := service.New(cfg, o.serviceOpts...)
	if err != nil {
		return nil, err
	}
	hdl := handler.FromService(svc)
```

改为：
```go
	svc, err := service.New(cfg, o.serviceOpts...)
	if err != nil {
		return nil, err
	}

	scheduler, err := jobs.New(&jobs.Deps{Cfg: cfg, Upload: svc.Upload()})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init jobs: %w", err), svc.Stop())
	}
	svc.AddExtra("jobs", scheduler)

	hdl := handler.FromService(svc)
```

如果 `errors` 和 `fmt` 还未 import，加进 import 块（实际现有代码已经在用 `errors.Join` 和 `fmt.Errorf`，应该已经 import）。

- [ ] **Step 3: 修改 pkg/module.go，装配 jobs**

修改 `pkg/module.go`：

整体替换 `NewModule` 函数：

把
```go
package pkg

import (
	"storage-service/pkg/config"
	"storage-service/pkg/handler"
	"storage-service/pkg/option"
)

// NewModule creates a Handler for in-process use. The Handler satisfies both
// storagev1.StorageServiceServer (call RPC methods directly) and signalx.Service
// (manage lifecycle). Callers that inject resources via options own those
// resources' lifecycle; Handler.Stop only releases resources it created.
func NewModule(cfg *config.Config, opts ...option.Option) (*handler.Handler, error) {
	return handler.New(cfg, opts...)
}
```

改为：
```go
package pkg

import (
	"errors"
	"fmt"

	"storage-service/internal/jobs"
	"storage-service/internal/service"
	"storage-service/pkg/config"
	"storage-service/pkg/handler"
	"storage-service/pkg/option"
)

// NewModule creates a Handler for in-process use. The Handler satisfies both
// storagev1.StorageServiceServer (call RPC methods directly) and signalx.Service
// (manage lifecycle). Callers that inject resources via options own those
// resources' lifecycle; Handler.Stop only releases resources it created.
//
// jobs.Scheduler is constructed here (not inside service.New) so the jobs
// package stays a peer of service rather than a dependency of it; the
// scheduler is attached to the service lifecycle.Manager via AddExtra.
func NewModule(cfg *config.Config, opts ...option.Option) (*handler.Handler, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}

	scheduler, err := jobs.New(&jobs.Deps{Cfg: cfg, Upload: svc.Upload()})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init jobs: %w", err), svc.Stop())
	}
	svc.AddExtra("jobs", scheduler)

	return handler.FromService(svc), nil
}
```

- [ ] **Step 4: 编译，确认通过**

Run:
```bash
go build ./...
```
Expected: 编译通过。

- [ ] **Step 5: 跑全套测试 + lint**

Run:
```bash
go test -race ./...
golangci-lint run ./...
```
Expected: 所有测试 PASS，lint 无报错。

如果 lint 报 `unused` 之类（比如某个 import 还在但不再用），按提示修正。

- [ ] **Step 6: Commit**

```bash
git add pkg/option/option.go pkg/server.go pkg/module.go
git commit -m "$(cat <<'EOF'
feat(server,module): wire jobs.Scheduler; drop option.WithCron

NewServer and NewModule construct jobs.Scheduler after service.New and
attach it via svc.AddExtra("jobs", ...). option.WithCron (and Options.Cron
field) removed — cron injection now lives at the jobs layer via
jobs.WithCron, which is in an internal package.
EOF
)"
```

---

## Task 6: 最终验证 + 文档同步

**目标：** 跑全套质量检查，确认整个改动闭环。更新 Obsidian changes 记录实施完成。

**Files:** 无源码改动（仅文档同步）

- [ ] **Step 1: 全套构建 + 测试 + lint**

Run:
```bash
go build ./...
go test -race -coverprofile=coverage.out ./...
golangci-lint run ./...
go vet ./...
```
Expected: 全部通过。覆盖率与改动前持平或更高（jobs 包新增覆盖率）。

- [ ] **Step 2: 验证 cron import 已完全从 service 段消失**

Run:
```bash
grep -rn "github.com/robfig/cron/v3" internal/service/ pkg/option/ pkg/server.go pkg/module.go
```
Expected: 无输出（service/option/server/module 不再 import cron）。`internal/jobs/jobs.go` 仍 import cron 是预期的。

- [ ] **Step 3: 验证 RunOnce / RegisterGC / cronComponent / resolveCron 已完全消失**

Run:
```bash
grep -rn "RunOnce\|RegisterGC\|cronComponent\|resolveCron" internal/ pkg/ cmd/ 2>/dev/null
```
Expected: 无输出。

- [ ] **Step 4: 验证 upload.Service 不再有 cron 字段**

Run:
```bash
grep -n "cron" internal/service/upload/upload.go internal/service/upload/reap.go
```
Expected: 无输出（upload 包完全不碰 cron）。

- [ ] **Step 5: 更新 Obsidian changes 记录实施完成**

执行：
```bash
obsidian vault=only append file="services/storage-service/changes" content="
- 2026-06-20: 完成 jobs 包实施 — upload session GC 迁出 upload.Service，新建 internal/jobs 包（Scheduler 拥有 cron），删除 service 的 cron 装配 + RunOnce facade + cronComponent/resolveCron，pkg/option.WithCron 移除（转移为 jobs.WithCron）"
```

- [ ] **Step 6: 推送（可选，问用户）**

如果用户确认推送：
```bash
git push origin feat/audit-logging
```

否则跳过。

---

## 完成判定

实施完成的判据：

1. `go build ./...` 通过
2. `go test -race ./...` 全绿
3. `golangci-lint run ./...` 无报错
4. `internal/jobs/jobs.go` 存在且提供 `Scheduler` 类型
5. `internal/service/upload/` 下没有 `gc.go`（只有 `reap.go`）
6. `internal/service/upload/upload.go` 中 `upload.Service` 无 `cron` 字段、无 `RegisterGC` 方法
7. `internal/service/helpers.go` 无 `cronComponent` / `resolveCron`
8. `internal/service/service.go` 无 `StorageService.cron` 字段、无 `RunOnce` 方法
9. `pkg/option/option.go` 无 `WithCron` 函数、无 `Options.Cron` 字段
10. `pkg/server.go` 和 `pkg/module.go` 都装配 `jobs.Scheduler` 并 `svc.AddExtra("jobs", ...)`
11. 6 个 commit，每个独立可编译，commit message 遵循 Conventional Commits

## 关联

- 设计 spec：[[services/storage-service/design/v1/jobs-package-design|Jobs Package Design]]
- 本地 spec：`docs/superpowers/specs/2026-06-20-jobs-package-design.md`
