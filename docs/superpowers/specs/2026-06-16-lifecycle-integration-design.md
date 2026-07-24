# Lifecycle 集成设计:storage-service 对齐 gid-service

日期:2026-06-16
分支:feat/audit-logging → 后续切到新分支
参考实现:gid-service(cmd/server + pkg + internal/service)

## 背景与动机

go-common 新增了 `lifecycle` 和 `signalx` 两个包,统一了服务的启停语义:

- `lifecycle.Manager` — 顺序 Start、并发 Stop(带超时),实现 `Service` 接口(`Start() error` / `Stop() error`)
- `signalx.Run` / `signalx.RunWithForceQuit` — Start → 等待 SIGINT/SIGTERM → Stop
- `grpcx.Server` — 已实现 `signalx.Service`,有 Start/Stop

gid-service 已完成改造,成为参考模式。storage-service 仍使用旧的 `Server.Run()` 自阻塞 + `Server.Stop()`(无返回值)模式,需要统一对齐。

## 目标

1. storage-service 的 cmd/server、pkg/server、internal/service 与 gid-service 模式完全对齐
2. 为未来后台任务(定时清理、metrics 推送等)预留 lifecycle.Manager 扩展位
3. 保持 NewModule 的 in-process API 形态(返回 `*StorageService`),仅方法名从 `Close()` 改为 `Stop()`
4. 不破坏 grpcx / client.go / 第三方调用的现有 Close 语义

## 当前状态分析

| 文件 | 当前 | 问题 |
|---|---|---|
| `cmd/server/main.go` | `srv.Run()` 自阻塞 | 无信号二次 SIGKILL;无统一 Start/Stop 错误聚合 |
| `pkg/server.go` | `Run()` 调 `grpcSrv.Run()`;`Stop()` 无返回值 | 不实现 signalx.Service;Stop 无法返回错误 |
| `internal/service/service.go` | 有 `Close()` 释放 db/redis,无 Start/Stop | 不实现 lifecycle 接口;无法被 Manager 统一管理 |

storage-service 当前**没有任何后台 goroutine**(`cleanup.go` 是手动调用,非定时任务),因此 lifecycle.Manager 启用后是空的,纯预留。

调用方影响:
- `cmd/server/main.go:29` `srv.Run()` — 需改
- `pkg/server.go:73` `s.svc.Close()` — 需改
- `pkg/module.go` 的 `NewModule` 返回值类型不变,in-process 调用方需将 `Close()` 改为 `Stop()`
- 测试代码未直接调用 NewModule/NewServer,不受影响

## 改造方案

### 1. `internal/service/service.go`

**结构体**:

```go
type StorageService struct {
    storagev1.UnimplementedStorageServiceServer

    db       *gorm.DB
    ownDB    bool
    redis    *redis.Client
    ownRedis bool
    // ... 其他字段不变
    manager *lifecycle.Manager  // 新增,目前空,预留扩展位
}
```

**`New()` 中初始化 Manager**:

```go
return &StorageService{
    // ... 其他字段
    manager: lifecycle.NewManager(),
}, nil
```

**新增 `Start()`**:

```go
// Start starts lifecycle-managed service internals.
func (s *StorageService) Start() error { return s.manager.Start() }
```

**`Close()` 改为 `Stop()`,返回 error,聚合 Manager 与资源清理**:

```go
// Stop stops lifecycle-managed internals and releases owned resources.
func (s *StorageService) Stop() error {
    var errs []error
    errs = append(errs, s.manager.Stop())

    if s.ownDB && s.db != nil {
        if sqlDB, err := s.db.DB(); err == nil {
            if err := sqlDB.Close(); err != nil {
                errs = append(errs, fmt.Errorf("close db: %w", err))
            }
        } else {
            errs = append(errs, fmt.Errorf("get sql db: %w", err))
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

> 注意:当前 `Close()` 用 `_ =` 忽略错误,改造后显式收集,符合 CLAUDE.md 中"禁止用 `_ =` 忽略 error"的约定。Close 本身作为资源清理保留在 Stop 内部,不再是独立方法。

### 2. `pkg/server.go`

**结构体**:移除 `cfg` 字段(不再需要打日志)。

**`NewServer`**:返回值构造不再带 cfg。

**删除 `Run()` 方法,新增 `Start()`**:

```go
// Start starts service internals and gRPC + HTTP gateway without blocking.
func (s *Server) Start() error {
    if err := s.svc.Start(); err != nil {
        return err
    }
    if err := s.grpcSrv.Start(); err != nil {
        return errors.Join(err, s.svc.Stop())
    }
    return nil
}
```

**`Stop()` 改为返回 error**:

```go
// Stop gracefully stops all transports and service internals.
func (s *Server) Stop() error {
    return errors.Join(s.grpcSrv.Stop(), s.svc.Stop())
}
```

> 与 gid-service `pkg/server.go` 完全一致,包括"grpc 启动失败时回滚 svc.Stop"的错误聚合模式。

### 3. `cmd/server/main.go`

```go
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

> import 新增 `github.com/servekit/go-common/signalx`。RunWithForceQuit 比 Run 多一个特性:第二次 Ctrl+C 直接 SIGKILL,避免长 Stop 卡死。

### 4. `pkg/module.go`

签名不变。in-process 模式示例:

```go
m, _ := storageservice.NewModule(cfg)
defer func() { _ = m.Stop() }()  // 之前是 m.Close()
```

## 不改造的部分

- `pkg/client.go` 的 `Close()` — 客户端语义,与服务端 lifecycle 无关
- `internal/thirdcall/gid_service/grpc.go` 的 `Close()` — 同上
- `internal/thirdcall/gid_service/module.go` 的 `NewModule` — 已经是 gid-service 模式
- `cmd/migrate/` — 一次性命令,不走 lifecycle

## 测试策略

1. **现有单元测试**:`internal/service/*_test.go` 直接构造 StorageService,不调 Start/Stop,无需改动
2. **新增 lifecycle 测试**:
   - `service_test.go`(或 `lifecycle_test.go`)— 验证 Start/Stop 多次调用幂等(Manager 内部 sync.Once 保证)
   - 验证 Stop 聚合错误(db/redis 关闭失败的 mock 场景,可用 sqlmock + miniredis 制造失败)
3. **Server 层**:可参考 gid-service 的 `pkg/server_test.go`(若已存在),用 bufconn 启动 gRPC 后 Stop 验证无残留
4. **手动验证**:`go run ./cmd/server/`,发 SIGTERM 验证 graceful shutdown,再发第二次 SIGTERM 验证强制退出

## 兼容性影响

| 调用方 | 影响 | 处理 |
|---|---|---|
| storage-service 自己的 cmd/server | 中 | main.go 改 |
| storage-service 的 pkg/server.go | 中 | 改 Run/Stop |
| storage-service 的 internal/service | 中 | 加 Start,Close→Stop |
| 下游 in-process 用户(若存在) | 小 | 把 `m.Close()` 改成 `m.Stop()` |
| gRPC 客户端 | 无 | client.Close 不变 |

storage-service 是基础设施服务,目前下游 in-process 用法仅在 storage-service 内部测试,无外部消费者需要通知。

## 风险

1. **Stop 顺序**:gid-service 模式是 `grpcSrv.Stop() || svc.Stop()` 并发聚合。storage-service 的 svc.Stop 会关 db,如果 gRPC 还在处理请求可能会失败。但 `grpcSrv.Stop()` 是 `GracefulStop`,会等所有 in-flight 请求完成才返回,因此 errors.Join 时 db 关闭发生在 grpc 停止之后(并发但实际有依赖)。**评估**:errors.Join 是逻辑上聚合,实际 Stop() 在 grpcx.Server.Stop 内是阻塞的 GracefulStop,所以 svc.Stop 内的 db.Close 不会被 in-flight 请求撞到。无需特殊处理。
2. **Start 失败回滚**:gid-service 在 grpc.Start 失败时调 svc.Stop(),storage-service 同样照搬。当前 svc.Stop 因为 Manager 空,只关 db/redis,不会出问题。
3. **in-process module 行为变化**:之前 NewModule 后 Close() 不返回 error,现在 Stop() 返回 error。调用方需要捕获或忽略。已在测试策略中说明。

## 关联

**实现计划:** 待 writing-plans skill 生成
