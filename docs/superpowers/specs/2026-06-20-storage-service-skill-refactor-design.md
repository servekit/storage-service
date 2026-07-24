# storage-service 对齐 golang-service-development skill 重构

**日期**：2026-06-20
**状态**：已批准，待实施
**关联 skill**：`ai-kit-studio/skills/golang-service-development`

## 背景

`golang-service-development` skill 定义了本团队所有 Go 微服务的架构层规则（目录布局、`pkg/handler` ↔ `internal/service` 分层、三态运行、thirdcall 双层、`lifecycle.Manager` 资源管理、proto enum 处理）。

storage-service 当前实现有多处偏离 skill 的反模式，本重构把这些反模式按 skill 推荐方式修正。

## 现状 gap

| # | Gap | skill 章节 | 影响 |
|---|-----|-----------|------|
| 1 | `pkg/handler/` 不存在；`StorageService` 直接 embed `UnimplementedStorageServiceServer`，gRPC stub（28 个）+ 业务方法混在一个 struct | §1 反模式 | handler/service 边界消失，未来替换 gRPC 框架或 in-process 调用会受牵连 |
| 2 | `ownDB / ownRedis / ownCron` bool 字段 | §5 | 资源一多就爆炸（`ownX` × N），且与已存在的 `mgr *lifecycle.Manager` 并存导致两套生命周期 |
| 3a | upload 域拆成 6 个文件：`upload.go` / `batch_upload.go` / `uploadtoken.go` / `upload_gc.go` / `cancel_upload.go` / `cleanup.go`（合计 ~1200 行） | §2 反例 | 按子主题拆而非按领域拆 |
| 3b | audit 域拆成 2 个文件：`audit.go` (357) + `audit_snapshots.go` (50) | §2 标准反例 | skill 文档原话点名 |
| 4 | `grpcx.New` 只传 3 个参数，缺：HTTP gateway 注册函数、ErrorInterceptor、protovalidate middleware | §7 | gateway 不启动；xerr 错误变 `codes.Unknown`；protovalidate 规则不生效 |

## 重构目标

按 skill 推荐方式修正上述 gap，且**业务行为不变**。`make test`（race + coverage）每个 Phase 后必须保持原覆盖基线。

## 设计

### Phase 1 — 抽 `pkg/handler`（结构隔离，行为不变）

**新增 `pkg/handler/storage.go`**：

```go
package handler

type Handler struct {
    storagev1.UnimplementedStorageServiceServer
    svc *service.StorageService
}

func (h *Handler) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
    return h.svc.GenerateUploadURL(ctx, req)
}
// ... 28 个一行委托
```

**改动**：

- `internal/service/service.go`
  - 删除 `storagev1.UnimplementedStorageServiceServer` embed
  - 删除 200-328 行所有 gRPC stub 方法（`func (s *StorageService) GenerateUploadURL` 等 28 个）
  - 业务方法从私有 `generateUploadURL` 改为导出 `GenerateUploadURL`（service 不再需要 stub+private 成对，skill §1 明确禁止这种成对反模式）
- `pkg/server.go`
  - `Server` 持有 `*handler.Handler` 而非 `*service.StorageService`
  - `NewServer` 构造 `handler.New(svc)` 后注册到 gRPC server
  - `Start/Stop` 转发：`hdl.Start()` → `svc.Start()`；`hdl.Stop()` → `svc.Stop()`
  - `Handler` 实现 `signalx.Service` 接口（skill §5 末段要求）
- `pkg/module.go`
  - 返回类型从 `*service.StorageService` 改为 `*handler.Handler`
  - in-process 调用方拿 Handler 即可，不需要 service handle（skill §3 要求）

**测试**：
- `internal/service/service_test.go` 全部继续通过（测试的是业务方法，不走 gRPC stub）
- 业务方法从 `s.generateUploadURL` 改为 `s.GenerateUploadURL` 后，测试调用点同步改名
- 新增 `pkg/handler/storage_test.go`（可选）：用 gomock 或 stub service 验证 Handler 纯委托

### Phase 2 — `ownX bool` → `lifecycle.Manager`

`StorageService` struct 删除 `ownDB / ownRedis / ownCron` 三个 bool 字段。所有资源统一注册到已有的 `mgr *lifecycle.Manager`：

| 资源 | 注册方式 | 说明 |
|------|---------|------|
| DB pool | `mgr.AddStopper("db", lifecycle.StopFunc(...))` | close 错误用 `slog.Warn` 记，skill §5 明确要求 |
| Redis | `mgr.AddStopper("redis", lifecycle.StopFunc(...))` | 同上 |
| GID gRPC client | `mgr.AddStopper("gid", lifecycle.StopFunc(...))` | 仅当实现了 `interface{ Close() error }` |
| Cron | 写 `cronComponent` 实现 `lifecycle.Service` | Start 启动 scheduler；Stop 调 `cron.Stop()` 等 `<-Done()` |

`resolveDB / resolveRedis / resolveGID / resolveCron` 改：
- **调用方注入** → 直接返回，**不注册**到 mgr → 调用方负责清理
- **调用方没注入** → 从 cfg 自建 + 注册到 mgr → Stop 按 LIFO 反序清理

`Service.Stop()` 简化为 `return s.mgr.Stop()`（cron 的 in-flight 等待由 `cronComponent.Stop` 内部处理，不再是 service 层的事）。

**测试**：
- `service_test.go` 现有断言（owned/injected 行为）需同步：原本检查 `ownDB==true` 之类的改为检查 mgr 是否注册了对应 Stopper，或简化为黑盒（注入的不 close，自建的 close）
- 资源清理测试：注入 DB → service.Stop 后 DB 仍可用；自建 DB → service.Stop 后 DB 关闭

### Phase 3 — 域文件合并

**audit 域**：合并 `audit.go` (357) + `audit_snapshots.go` (50) → `audit.go` (407)。407 行 < 500 阈值，不升级子包。

**upload 域**：升级为 `internal/service/upload/` 子包（合计 ~1200 行 > 500 阈值，skill §2 要求升级）：

```
internal/service/upload/
├── upload.go     # Service 子 struct + 主入口（service 包调用的公开方法）
├── batch.go      # BatchGetSTSCredential / batch 上传
├── cancel.go     # CancelUpload
├── gc.go         # upload_gc.go + cleanup.go 合并：后台 GC + cron 注册
├── token.go      # uploadtoken.go 迁入
└── sts.go        # STS 凭证生成（从 sts_cache.go 迁入；sts_cache_test.go → upload/sts_test.go）
```

**service.go facade 方法**（skill §2 判据：领域升级成子包时必须写 facade）：

```go
type StorageService struct {
    // ...
    upload *upload.Service  // 子包实例
}

func (s *StorageService) GenerateUploadURL(ctx, req) (*resp, error) {
    return s.upload.GenerateUploadURL(ctx, req)
}
// ... 所有原 upload 域方法都委托给 s.upload.*
```

handler 永远只调 `service.go` 暴露的方法，不直接 import `internal/service/upload` 子包（skill §2 强制约束）。

**测试**：
- upload 相关测试从 `internal/service/upload_gc_test.go` / `service_test.go` 中的 upload 部分迁到 `internal/service/upload/upload_test.go`
- audit 相关测试保留在 `service_test.go`（audit 仍在主包）

### Phase 4 — `grpcx.New` 三件套补齐

```go
grpcSrv := grpcx.New(
    grpcx.ServerConfig{GRPCAddr: cfg.Server.GRPCAddr, GatewayAddr: cfg.Server.HTTPAddr},
    func(gs *grpc.Server) { storagev1.RegisterStorageServiceServer(gs, hdl) },
    storagev1.RegisterStorageServiceHandlerFromEndpoint,  // 启用 HTTP gateway
    grpcx.ErrorInterceptor,                                // xerr → gRPC status
    protovalidate_middleware.UnaryServerInterceptor(validator),
)
```

**依赖**：检查 `go-common` 是否已封装 protovalidate middleware；未封装则引入 `github.com/bufbuild/protovalidate-go` + middleware。

**验收**（skill §9）：
- `make proto && git diff --exit-code` 生成结果与 committed 一致
- `make build` 通过
- `make run` + grpcurl CreateDemo 等价物（Upload / GetMyFile 等）跑通
- curl HTTP gateway 跑通
- in-process module 测试（`pkg.NewModule`）跑通

## PR 节奏

四个 Phase 各自独立 commit，每个 commit 后 `make test` 必须通过：

1. `refactor(handler): extract pkg/handler as gRPC thin shell` — Phase 1
2. `refactor(service): replace ownX bool with lifecycle.Manager` — Phase 2
3. `refactor(service): merge audit files and upgrade upload to subpackage` — Phase 3
4. `feat(server): enable HTTP gateway, error interceptor, protovalidate` — Phase 4

## 非目标（本次不做）

- `pkg/thirdcall/gid_service.go` 文件命名调整（skill 未强制 `pkg/thirdcall/` 内文件命名，现状可接受）
- `internal/provider/` 改名（这里 provider 指存储 provider，非 skill §1 中"mqtt/kafka/jobs 辅助业务"的语义，不冲突）
- 已有业务逻辑、proto 定义、DB 模型不动

## 风险

- **Phase 1**：业务方法从私有改导出，可能漏改测试调用点 → 用 `go build ./...` + `go test ./...` 兜底
- **Phase 2**：`lifecycle.Manager` 的 Stop 顺序与原 `ownCron → ownDB → ownRedis` 是否一致 → mgr 按 LIFO（注册反序），需保证注册顺序与原 Stop 顺序等价
- **Phase 3**：upload 子包拆分时，包间共享的辅助函数（如 `internalError`）需要决定留在主包还是下移 → 优先留在 `service.go` / `common.go`，子包通过参数传入
- **Phase 4**：启用 protovalidate 可能让原本没被服务层校验的字段开始返回 400 → 检查 proto 中所有 `(buf.validate.field)` 注解，确认 service 层是否已校验同等内容

## 关联

- **设计依据**：`ai-kit-studio/skills/golang-service-development/golang-service-development.md`
- **历史 spec**：`docs/superpowers/specs/2026-06-16-lifecycle-integration-design.md`（lifecycle 已部分集成，本次完整切换）
- **同级 skills**：golang-development、gorm-cli-development、proto-development
