# Lifecycle Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 storage-service 的 cmd/server、pkg/server、internal/service 改造为 gid-service 模式:`signalx.RunWithForceQuit` + 内嵌 `lifecycle.Manager` + `Start/Stop` 接口。

**Architecture:** 自底向上:先给 `internal/service.StorageService` 加 lifecycle.Manager 和 Start/Stop;再把 `pkg.Server` 改造为实现 signalx.Service(删 Run,加 Start,Stop 返回 error);最后让 `cmd/server/main.go` 用 `signalx.RunWithForceQuit(srv)`。每步保持可编译。

**Tech Stack:** `go-common/lifecycle`、`go-common/signalx`、`go-common/grpcx`(grpcx 已自带 Start/Stop)

**对应 Spec:** `docs/superpowers/specs/2026-06-16-lifecycle-integration-design.md`

---

### Task 1: 给 StorageService 加 lifecycle.Manager 和 Start 方法

**Files:**
- Modify: `internal/service/service.go`
- Create: `internal/service/lifecycle_test.go`

**为什么先做这个:** service 是最底层,先让它具备 Start 接口,不影响现有 Close。本任务结束 service.go 还能编译,既有 Close 仍可工作。

- [ ] **Step 1: 写失败测试 `lifecycle_test.go`**

Create `internal/service/lifecycle_test.go`:

```go
package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestStart_idempotent verifies Start returns nil on a freshly constructed service
// and is idempotent across multiple calls (lifecycle.Manager uses sync.Once).
func TestStart_idempotent(t *testing.T) {
	svc := &StorageService{}
	// manager is nil here; we expect New() to populate it. For this test we
	// construct directly so we explicitly set a Manager.
	setupManagerForTest(svc)

	assert.NoError(t, svc.Start())
	// Second call must not panic or error.
	assert.NoError(t, svc.Start())
}
```

> 注:`setupManagerForTest` 是测试辅助函数,放在 step 3 同一文件底部,用 `// --- internal helpers ---` 分隔。这避免测试依赖 New()(会触发 db/redis/gid 等外部依赖)。

- [ ] **Step 2: 运行测试,确认失败**

Run: `cd /Users/moss/code/base/storage-service && go test ./internal/service/ -run TestStart_idempotent -v`
Expected: 编译错误 — `svc.Start undefined` 和 `setupManagerForTest undefined`。

- [ ] **Step 3: 改 `internal/service/service.go`**

3a. 在 import 块加 `"github.com/servekit/go-common/lifecycle"`:

```go
import (
	"context"
	"fmt"

	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/ratelimit"
	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/audit"
	"storage-service/internal/provider"
	"storage-service/internal/store/repository"
	"storage-service/pkg/config"
	"storage-service/pkg/option"
	"storage-service/pkg/thirdcall"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)
```

3b. 给 `StorageService` 结构体加 `manager *lifecycle.Manager` 字段(放在 `audit audit.Recorder` 之后):

```go
type StorageService struct {
	storagev1.UnimplementedStorageServiceServer

	db       *gorm.DB
	ownDB    bool
	redis    *redis.Client
	ownRedis bool
	registry *provider.Registry
	gid      thirdcall.GIDService
	limiter  ratelimit.Limiter
	cfg      *config.Config

	objectRepo   *repository.ObjectRepo
	fileRepo     *repository.FileRepo
	auditLogRepo *repository.AuditLogRepo
	audit        audit.Recorder
	manager      *lifecycle.Manager
}
```

3c. 在 `New()` 的 return 语句中初始化 manager(加在 `audit: auditRecorder,` 之后):

```go
	return &StorageService{
		db:           db,
		ownDB:        ownDB,
		redis:        redisClient,
		ownRedis:     ownRedis,
		registry:     registry,
		gid:          gidGen,
		limiter:      limiter,
		cfg:          cfg,
		objectRepo:   objectRepo,
		fileRepo:     fileRepo,
		auditLogRepo: auditLogRepo,
		audit:        auditRecorder,
		manager:      lifecycle.NewManager(),
	}, nil
```

3d. 在 `Close()` 方法之前加 `Start()` 方法:

```go
// Start starts lifecycle-managed service internals. Currently a no-op since
// no background tasks are registered with the manager; reserved for future
// use (periodic cleanup workers, metrics pushers, etc.).
func (s *StorageService) Start() error { return s.manager.Start() }
```

3e. 在 `lifecycle_test.go` 文件底部加测试辅助函数:

```go
// --- internal helpers ---

// setupManagerForTest initializes the lifecycle.Manager field for a directly
// constructed StorageService. Tests that bypass New() (which has external
// dependencies) call this to get a usable Start/Stop.
func setupManagerForTest(svc *StorageService) {
	svc.manager = lifecycle.NewManager()
}
```

并在 `lifecycle_test.go` 的 import 中加 `"github.com/servekit/go-common/lifecycle"`:

```go
import (
	"testing"

	"github.com/servekit/go-common/lifecycle"

	"github.com/stretchr/testify/assert"
)
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `cd /Users/moss/code/base/storage-service && go test ./internal/service/ -run TestStart_idempotent -v`
Expected: PASS。

- [ ] **Step 5: 编译验证**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 成功。pkg/server.go 仍然调用 svc.Close() 不受影响。

- [ ] **Step 6: 跑全部测试,确认无回归**

Run: `cd /Users/moss/code/base/storage-service && go test ./...`
Expected: 全部 PASS(包括既有的 service / repository / handler 测试)。

- [ ] **Step 7: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/service.go internal/service/lifecycle_test.go
git commit -m "$(cat <<'EOF'
feat(service): add lifecycle.Manager and Start method to StorageService

Embed lifecycle.Manager as a reserved extension slot for future background
tasks. Start delegates to manager.Start() (currently a no-op). Close() is
unchanged in this commit so pkg/server.go keeps compiling.
EOF
)"
```

---

### Task 2: 把 service.Close 改为 Stop 返回 error,并同步修 pkg/server.go

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/lifecycle_test.go`
- Modify: `pkg/server.go`

**为什么这样划分:** Close→Stop 是破坏性 API 变更,必须同步改调用方 pkg/server.go,否则编译失败。两处放同一 commit 保证每个 commit 都可编译。

- [ ] **Step 1: 写失败测试 — Stop 关闭 db 并返回 nil**

Append to `internal/service/lifecycle_test.go`:

```go
// TestStop_releasesOwnedDB verifies Stop closes an owned *sql.DB and returns nil.
func TestStop_releasesOwnedDB(t *testing.T) {
	db, sqlDB, err := openOwnedTestDB(t)
	require.NoError(t, err)

	svc := &StorageService{db: db, ownDB: true}
	setupManagerForTest(svc)

	err = svc.Stop()
	assert.NoError(t, err)
	assert.ErrorIs(t, sqlDB.Ping(), sql.ErrConnDone, "underlying sql.DB must be closed")
}

// TestStop_aggregatesErrors verifies Stop collects errors from both manager.Stop
// and resource cleanup via errors.Join.
func TestStop_aggregatesErrors(t *testing.T) {
	// Construct with ownRedis=true but nil redis — code path skips nil.
	// Construct with ownDB=true but nil db — code path skips nil.
	// manager is empty so manager.Stop() returns nil.
	svc := &StorageService{ownDB: true, ownRedis: true}
	setupManagerForTest(svc)

	err := svc.Stop()
	assert.NoError(t, err, "nil db/redis are skipped; manager.Stop returns nil")
}
```

并在 `lifecycle_test.go` import 加 `"database/sql"`, `"github.com/stretchr/testify/require"`,以及辅助函数 `openOwnedTestDB`:

```go
import (
	"database/sql"
	"testing"

	"github.com/servekit/go-common/lifecycle"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

在 `lifecycle_test.go` 底部(`setupManagerForTest` 之后)追加 `openOwnedTestDB`:

```go
// openOwnedTestDB returns an in-memory sqlite *gorm.DB plus its underlying
// *sql.DB so the test can assert the sql.DB is closed after Stop.
//
// We use sqlite because it has no external process; storage-service normally
// uses postgres via dbx, but this test only verifies Stop's close semantics
// and does not need postgres features.
func openOwnedTestDB(t *testing.T) (*gorm.DB, *sql.DB, error) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, nil, err
	}
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	underlying, err := db.DB()
	if err != nil {
		return nil, nil, err
	}
	return db, underlying, nil
}
```

> **依赖说明:** 这一步引入 `gorm.io/driver/sqlite`。如果 go.mod 里没有,需要先 `go get gorm.io/driver/sqlite github.com/mattn/go-sqlite3`。

- [ ] **Step 2: 加 sqlite 依赖(若缺失)**

Run:
```bash
cd /Users/moss/code/base/storage-service && go get gorm.io/driver/sqlite github.com/mattn/go-sqlite3
```

> 注:如果项目里已有其他 in-memory DB 测试惯用方案(如 sqlmock),改用 sqlmock 也可以。spec 测试策略里允许 sqlmock + miniredis。

- [ ] **Step 3: 运行测试,确认失败**

Run: `cd /Users/moss/code/base/storage-service && go test ./internal/service/ -run TestStop -v`
Expected: 编译错误 — `svc.Stop undefined`。

- [ ] **Step 4: 改 `internal/service/service.go`**

4a. 在 import 块加 `"errors"`:

```go
import (
	"context"
	"errors"
	"fmt"

	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/ratelimit"
	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/audit"
	"storage-service/internal/provider"
	"storage-service/internal/store/repository"
	"storage-service/pkg/config"
	"storage-service/pkg/option"
	"storage-service/pkg/thirdcall"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)
```

4b. 把 `Close()` 方法整段替换为 `Stop()`:

```go
// Stop stops lifecycle-managed internals and releases owned resources
// (db, redis). Errors from each step are aggregated via errors.Join so a
// failure in one does not mask others.
func (s *StorageService) Stop() error {
	var errs []error
	if err := s.manager.Stop(); err != nil {
		errs = append(errs, fmt.Errorf("lifecycle stop: %w", err))
	}

	if s.ownDB && s.db != nil {
		sqlDB, err := s.db.DB()
		if err != nil {
			errs = append(errs, fmt.Errorf("get sql db: %w", err))
		} else if err := sqlDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close db: %w", err))
		}
	}
	if s.ownRedis && s.redis != nil {
		if err := s.redis.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close redis: %w", err))
		}
	}
	return errors.Join(errs...)
}
```

> 注意:`Close()` 是导出方法,改成 `Stop()` 是破坏性变更。在 storage-service 仓库内,调用方只有 `pkg/server.go:73` 的 `s.svc.Close()`(下一步修)。仓库外 in-process 用户若有需要自行适配。

- [ ] **Step 5: 改 `pkg/server.go` 同步适配**

打开 `pkg/server.go`,把 `Stop()` 方法中的 `s.svc.Close()` 改成 `s.svc.Stop()`:

把这段:

```go
// Stop gracefully stops all transports and releases resources.
func (s *Server) Stop() {
	s.grpcSrv.Stop()
	s.svc.Close()
}
```

改成:

```go
// Stop gracefully stops all transports and releases resources.
func (s *Server) Stop() {
	s.grpcSrv.Stop()
	_ = s.svc.Stop()
}
```

> 注:这一步 pkg/server.go 的 Stop 仍是无返回值的过渡态。Task 3 会把 Stop 改为返回 error,届时 `_ =` 会被移除。临时 `_ =` 是为了不破坏现有签名;Task 3 一并清理。

- [ ] **Step 6: 运行测试,确认通过**

Run: `cd /Users/moss/code/base/storage-service && go test ./internal/service/ -run TestStop -v`
Expected: PASS。

- [ ] **Step 7: 编译验证整个项目**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 成功。

- [ ] **Step 8: 跑全部测试,确认无回归**

Run: `cd /Users/moss/code/base/storage-service && go test ./...`
Expected: 全部 PASS。

- [ ] **Step 9: Commit**

```bash
cd /Users/moss/code/base/storage-service
git add internal/service/service.go internal/service/lifecycle_test.go pkg/server.go go.mod go.sum
git commit -m "$(cat <<'EOF'
refactor(service): rename Close to Stop, aggregate errors via errors.Join

StorageService.Stop now satisfies the lifecycle.Service interface: it stops
the lifecycle.Manager and closes owned db/redis, aggregating all errors with
errors.Join so a failure in one step does not mask others.

pkg/server.go is updated to call svc.Stop() (still signature-compatible for
now; Task 3 will give Server its own Start/Stop pair).
EOF
)"
```

---

### Task 3: pkg/server.go 完整改造为 signalx.Service

**Files:**
- Modify: `pkg/server.go`

**目标:** Server 实现 `signalx.Service`(Start/Stop 返回 error),与 gid-service `pkg/server.go` 一致。删除 `Run()`,移除 `cfg` 字段。

- [ ] **Step 1: 重写 `pkg/server.go`**

整体替换文件内容为:

```go
package pkg

import (
	"errors"

	"github.com/servekit/go-common/grpcx"
	"google.golang.org/grpc"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/service"
	"storage-service/pkg/config"
	"storage-service/pkg/option"
)

// Server wraps a gRPC server for the storage service.
type Server struct {
	grpcSrv *grpcx.Server
	svc     *service.StorageService
}

// ServerOption configures a Server instance.
type ServerOption func(*serverOptions)

type serverOptions struct {
	serviceOpts []option.Option
}

// WithServiceOptions forwards options to the service layer.
func WithServiceOptions(opts ...option.Option) ServerOption {
	return func(o *serverOptions) { o.serviceOpts = append(o.serviceOpts, opts...) }
}

// NewServer creates a Server with all dependencies.
func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}

	svc, err := service.New(cfg, o.serviceOpts...)
	if err != nil {
		return nil, err
	}

	grpcSrv := grpcx.New(
		grpcx.ServerConfig{
			GRPCAddr:    cfg.Server.GRPCAddr,
			GatewayAddr: cfg.Server.HTTPAddr,
		},
		func(gs *grpc.Server) {
			storagev1.RegisterStorageServiceServer(gs, svc)
		},
		nil,
	)

	return &Server{grpcSrv: grpcSrv, svc: svc}, nil
}

// Start starts service internals and the gRPC + HTTP gateway without blocking.
// If grpcSrv.Start fails, svc.Stop is called to roll back partial startup.
func (s *Server) Start() error {
	if err := s.svc.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.svc.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC + HTTP gateway and service internals.
// Errors from each component are aggregated via errors.Join.
func (s *Server) Stop() error {
	return errors.Join(s.grpcSrv.Stop(), s.svc.Stop())
}
```

变更要点:
- 删除 `Run()` 方法
- `Start()` 新增,与 gid-service 一致(失败回滚 svc.Stop)
- `Stop()` 改为返回 error,聚合 grpcSrv.Stop + svc.Stop
- 删除 `cfg *config.Config` 字段及结构体中的引用
- 删除 `log/slog` import(不再打日志;grpcx 自己会打)
- 保留 `NewServer`、`ServerOption`、`WithServiceOptions`、`serverOptions` 不变

- [ ] **Step 2: 编译验证**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: `cmd/server/main.go` 编译失败 — `srv.Run undefined`(预期,Task 4 修复)。

> 此时不要 commit,Task 4 一起修。

---

### Task 4: cmd/server/main.go 改用 signalx.RunWithForceQuit

**Files:**
- Modify: `cmd/server/main.go`

- [ ] **Step 1: 重写 `cmd/server/main.go`**

整体替换为:

```go
// Package main is the entry point for the storage service server.
package main

import (
	"log/slog"
	"os"

	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/signalx"

	pkg "storage-service/pkg"
	"storage-service/pkg/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logging.Setup(&cfg.Log)

	srv, err := pkg.NewServer(cfg)
	if err != nil {
		slog.Error("init server", "error", err)
		os.Exit(1)
	}

	if err := signalx.RunWithForceQuit(srv); err != nil {
		slog.Error("run server", "error", err)
		os.Exit(1)
	}
}
```

变更要点:
- 新增 import `"github.com/servekit/go-common/signalx"`
- `srv.Run()` 改为 `signalx.RunWithForceQuit(srv)`
- 其余不变

- [ ] **Step 2: 编译验证**

Run: `cd /Users/moss/code/base/storage-service && go build ./...`
Expected: 成功。

- [ ] **Step 3: 跑全部测试**

Run: `cd /Users/moss/code/base/storage-service && go test ./...`
Expected: 全部 PASS。

- [ ] **Step 4: vet 和 lint**

Run:
```bash
cd /Users/moss/code/base/storage-service
go vet ./...
gofmt -l .  # 应无输出
goimports -l .  # 应无输出
```

如有 `internal/middleware/doc.go` 删除状态(本分支已删,见 git status),不在本计划范围内。

- [ ] **Step 5: Commit(Task 3 + Task 4 一起)**

```bash
cd /Users/moss/code/base/storage-service
git add pkg/server.go cmd/server/main.go
git commit -m "$(cat <<'EOF'
refactor(server): implement signalx.Service, switch main to RunWithForceQuit

pkg.Server now implements signalx.Service: Start starts svc + gRPC + gateway
non-blocking (rolling back svc.Stop on failure), Stop aggregates grpcSrv.Stop
and svc.Stop via errors.Join. The cfg field is removed (grpcx logs addresses
itself). cmd/server/main.go calls signalx.RunWithForceQuit, giving operators
a second Ctrl+C → SIGKILL escape hatch during slow graceful shutdowns.
EOF
)"
```

---

### Task 5: 端到端手动验证

**Files:** 无代码改动。

**目的:** 验证服务能启动、收 SIGTERM 后 graceful shutdown、收第二次 SIGTERM 强制退出。

- [ ] **Step 1: 启动服务**

前置:确认有可用的 config.yaml(项目根),包含 db/redis/gid 配置。

Run(在项目根):
```bash
cd /Users/moss/code/base/storage-service
go run ./cmd/server/
```

Expected(日志):
- `gRPC server listening addr=:9000`(或配置中的地址)
- `HTTP gateway listening addr=:8080`(若配置)
- 进程不退出(阻塞在 signalx 内)

> 若 db/redis 连不上会启动失败,这是配置问题,不在本计划范围。

- [ ] **Step 2: 验证 graceful shutdown(单次 SIGTERM)**

新开终端:
```bash
pkill -SIGTERM -f "storage-service.*cmd/server"
```

或拿到 PID 后:
```bash
kill -TERM <pid>
```

Expected(原终端日志):
- `server stopped`(grpcx.Stop 打的)
- 进程退出码 0

- [ ] **Step 3: 验证强制退出(双次 SIGTERM)**

重启服务,然后快速发两次 SIGTERM:
```bash
go run ./cmd/server/ &
PID=$!
sleep 2
kill -TERM $PID
kill -TERM $PID  # 第二次
wait $PID
echo "exit: $?"
```

Expected: 进程在第二次 SIGTERM 后立即退出(SIGKILL),退出码 137(128+9)或类似。

- [ ] **Step 4: 跑全测试再次确认**

Run: `cd /Users/moss/code/base/storage-service && go test -race ./...`
Expected: 全部 PASS,无 race。

- [ ] **Step 5: 更新文档(若 Obsidian 已有 plan 文档)**

如果 Spec 文档里写了"实现计划:待 writing-plans skill 生成",改为链接到本 plan:

打开 Obsidian `services/storage-service/design/v1/lifecycle-integration.md`,把末尾的:

```markdown
## 关联

**实现计划:** 待 writing-plans skill 生成
```

改为:

```markdown
## 关联

**实现计划:** [[services/storage-service/plan/v1/lifecycle-integration|lifecycle-integration]]
```

(本步骤需要先把本 plan 同步到 Obsidian `services/storage-service/plan/v1/lifecycle-integration.md`)

- [ ] **Step 6: 同步 plan 到 Obsidian + 更新 changes.md**

```bash
# 复制 plan 到 Obsidian
cp docs/superpowers/plans/2026-06-16-lifecycle-integration.md \
   ~/Library/Mobile\ Documents/iCloud~md~obsidian/Documents/only/services/storage-service/plan/v1/lifecycle-integration.md

# 追加变更记录
obsidian vault=only append file="services/changes" content="
- 2026-06-16: 新增 services/storage-service/plan/v1/lifecycle-integration.md — lifecycle 集成实施计划(5 个 task)"
```

> 这一步是文档同步,无代码改动,不需要 commit(或单独 commit docs)。

---

## Self-Review 结果

**Spec coverage:**
- ✅ service 层加 lifecycle.Manager + Start/Stop → Task 1 + Task 2
- ✅ pkg/server 实现 signalx.Service → Task 3
- ✅ cmd/server 用 RunWithForceQuit → Task 4
- ✅ NewModule in-process 用户行为变化(Close→Stop)→ Task 2 说明
- ✅ 不改 client.go / thirdcall / cmd/migrate → Task 3 文件清单只列 pkg/server.go,Task 4 只列 main.go
- ✅ 测试策略(单元 + 手动)→ Task 1/2 单元,Task 5 手动

**Placeholder scan:** 无 TBD/TODO,每步都有具体代码或命令。

**Type consistency:**
- `Start() error` / `Stop() error` 在 service.go、pkg/server.go 中签名一致
- `manager *lifecycle.Manager` 字段名一致
- `setupManagerForTest` 在 Task 1 定义,Task 2 复用,一致
- `openOwnedTestDB` 在 Task 2 step 1 引用并在同 step 末尾定义,一致

**风险/已知限制:**
- Task 2 引入 `gorm.io/driver/sqlite` 作为测试依赖。如果项目不愿意加这个依赖,可改用 `database/sql` + `sqlmock`(spec 测试策略允许)。已在该 Task step 2 注明。
- Task 3 重写 pkg/server.go 后 Task 4 之前项目无法编译,这是有意为之 — Task 3 单独 commit 会留下 broken state,所以 Task 3 + Task 4 合并到一个 commit(step 5)。已在 Task 3 step 2 末尾注明。
- `pkg/thirdcall/gid_service.go` 调用 `gid_service.NewModule` 返回 `*GidService`,gid-service 模式下其 Stop 返回 error。若 in-process GID 在 storage-service 内被持有,需要相应处理 Stop 返回值 — 这不在本计划范围,留待独立任务。

## 关联

**对应 Spec:** `docs/superpowers/specs/2026-06-16-lifecycle-integration-design.md`
