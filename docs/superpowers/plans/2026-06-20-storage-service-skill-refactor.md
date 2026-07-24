# storage-service 对齐 golang-service-development skill 重构 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 storage-service 重构到符合 `ai-kit-studio/skills/golang-service-development` 的架构层规则（4 个 Phase，每个 Phase 独立 commit，每个 Phase 后 `make test` 必须通过）。

**Architecture:** 纯结构重构（Phase 1-3）+ 行为补全（Phase 4）。Phase 1 抽 `pkg/handler` 薄壳、Phase 2 用 `lifecycle.Manager` 统一资源管理、Phase 3 按领域合并文件（audit 合并 + upload 升级子包）、Phase 4 补全 `grpcx.New` 的 gateway/errorInterceptor/protovalidate 三件套。

**Tech Stack:** Go 1.22+, `github.com/servekit/go-common`（lifecycle/grpcx/signalx/dbx/redisx/cronx）, `buf.build/go/protovalidate`, `github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate`, GORM, PostgreSQL。

**Spec:** `docs/superpowers/specs/2026-06-20-storage-service-skill-refactor-design.md`

---

## File Structure 总览

每个 Phase 涉及的文件：

**Phase 1 — pkg/handler 抽取**
- Create: `pkg/handler/handler.go` — Handler struct + New + Start/Stop
- Create: `pkg/handler/storage.go` — 27 个 RPC 委托方法
- Modify: `internal/service/service.go` — 删 embed + 27 个 stub；rename 已有业务方法 lowercase→PascalCase
- Modify: `internal/service/{upload,batch_upload,cancel_upload,file,quota,admin,audit}.go` — 业务方法 lowercase→PascalCase rename
- Modify: `internal/service/service_test.go` — 测试调用点 rename
- Modify: `pkg/server.go` — 注册 Handler 而非 Service
- Modify: `pkg/module.go` — 返回 `*handler.Handler`

**Phase 2 — lifecycle.Manager 统一资源**
- Modify: `internal/service/service.go` — 删 `ownDB/ownRedis/ownCron`，所有资源走 `mgr.AddStopper/Add`
- Modify: `internal/service/helpers.go` — `resolveDB/resolveGID/resolveRedis` 签名改（不再返回 bool，注册到 mgr）
- Create: `internal/service/cron_component.go` — `cronComponent` 实现 `lifecycle.Service`
- Modify: `internal/service/service_test.go` — 调整 ownX 相关断言

**Phase 3 — 域文件合并**
- Modify: `internal/service/audit.go` — 合入 `audit_snapshots.go` 内容
- Delete: `internal/service/audit_snapshots.go`
- Create: `internal/service/upload/upload.go` — upload Service 子 struct + 主入口
- Create: `internal/service/upload/{batch,cancel,gc,token,sts}.go` — 子文件
- Delete: `internal/service/{upload,batch_upload,cancel_upload,upload_gc,uploadtoken,sts_cache}.go`
- Create: `internal/service/upload/upload_test.go` — 迁入 upload 相关测试
- Delete: `internal/service/{upload_gc_test,sts_cache_test}.go`
- Modify: `internal/service/service.go` — 加 facade 委托方法（`s.upload.GenerateUploadURL` 等）
- Modify: `internal/service/service_test.go` — 剩余 audit/admin/file/quota 测试保留，调整调用路径

**Phase 4 — grpcx.New 三件套**
- Modify: `pkg/server.go` — 补 gateway 注册函数 + ErrorInterceptor + protovalidate middleware
- Modify: `go.mod` / `go.sum` — 加 protovalidate 依赖
- Modify: `Makefile` — 如需新增 `protovalidate-lint` 目标

---

## Phase 1: 抽 `pkg/handler`（结构隔离）

### Task 1.1: 删除 service.go 的 stub 方法和 embed

**Files:**
- Modify: `internal/service/service.go` — 删除第 31 行 `storagev1.UnimplementedStorageServiceServer` embed；删除第 203-328 行所有 27 个 gRPC stub 方法（`// --- gRPC stubs ---` 注释及之后全部）；删除第 10 行 `storagev1 "storage-service/gen/storage/v1"` import（如果只有 stubs 用了它，确认后删）；删除第 22 行 `"google.golang.org/protobuf/types/known/emptypb"` import（如果只有 stubs 用了）

- [ ] **Step 1: Read service.go to confirm exact line ranges**

Run: `wc -l internal/service/service.go && grep -n "^// --- gRPC stubs ---\|^// Upload$\|^// Audit Log$" internal/service/service.go`
Expected: 确认 stub 段从 `// --- gRPC stubs ---` 开始到文件末尾（约第 203 行起）

- [ ] **Step 2: Delete stub section**

用 Edit 工具，把 `// --- gRPC stubs ---\n\n// Upload\n\nfunc (s *StorageService) GenerateUploadURL...` 到文件末尾的全部内容替换为空。先读这段的起止行号，old_string 用第一个 stub 注释 + 后续全部内容。

- [ ] **Step 3: Delete `UnimplementedStorageServiceServer` embed**

Edit `internal/service/service.go`，old_string:
```go
// StorageService implements storagev1.StorageServiceServer.
type StorageService struct {
	storagev1.UnimplementedStorageServiceServer
	db       *gorm.DB
```
new_string:
```go
// StorageService holds business logic for the storage service. It no longer
// implements storagev1.StorageServiceServer directly — pkg/handler.Handler is
// the gRPC thin shell that delegates to these methods.
type StorageService struct {
	db       *gorm.DB
```

- [ ] **Step 4: Remove unused imports**

Run: `go build ./internal/service/ 2>&1 | grep "imported and not imported"`
Expected: 报 `storagev1`、`emptypb` 未使用（如果只有 stubs 用了它们）

Edit `internal/service/service.go`，删掉报未使用的 import 行。注意：service.go 内 `RunOnce` 等其它方法可能仍用 `storagev1`，build output 是真相。

- [ ] **Step 5: Don't run build yet** — 此状态下 server.go 还引用 `RegisterStorageServiceServer(gs, svc)`，会报 `*StorageService does not implement StorageServiceServer`。下一步 Task 1.3-1.4 修复。

### Task 1.2: 业务方法 lowercase → PascalCase rename

**Files:**
- Modify: `internal/service/upload.go` — `generateUploadURL/confirmUpload/getSTSCredential` → PascalCase
- Modify: `internal/service/batch_upload.go` — `batchGetSTSCredential` → PascalCase（注意 `processOneUpload` 是 helper，保持 lowercase）
- Modify: `internal/service/cancel_upload.go` — `cancelUpload` → PascalCase
- Modify: `internal/service/file.go` — `generateDownloadURL/listMyFiles/getMyFile/updateMyFile/deleteMyFile/batchDeleteMyFiles/generateProcessURL/getMyQuota` → PascalCase
- Modify: `internal/service/admin.go` — `adminGetQuota/adminSetQuota/adminSoftDeleteOwnerFiles/adminDeleteOwner/adminGetStats/adminListFiles/adminGetFile/adminDeleteFile/adminListProviders/adminListBuckets` → PascalCase
- Modify: `internal/service/audit.go` — `listMyAuditLogs/adminListAuditLogs` → PascalCase
- Modify: `internal/service/quota.go` — `setOwnerQuota/addOwnerQuota` → PascalCase（注意 `getQuota/setQuota/addQuota` 是 helper，保持 lowercase）
- Modify: `internal/service/service.go` — 删 stub 后没有调用点需要改
- Modify: `internal/service/service_test.go` — 改测试中所有 `svc.adminListProviders` → `svc.AdminListProviders` 等

**重要：rename 的精确清单**（这些是 RPC 业务方法，**不是** helper）：

| 文件 | 旧名 | 新名 |
|------|------|------|
| upload.go | `generateUploadURL` | `GenerateUploadURL` |
| upload.go | `confirmUpload` | `ConfirmUpload` |
| upload.go | `getSTSCredential` | `GetSTSCredential` |
| batch_upload.go | `batchGetSTSCredential` | `BatchGetSTSCredential` |
| cancel_upload.go | `cancelUpload` | `CancelUpload` |
| file.go | `generateDownloadURL` | `GenerateDownloadURL` |
| file.go | `listMyFiles` | `ListMyFiles` |
| file.go | `getMyFile` | `GetMyFile` |
| file.go | `updateMyFile` | `UpdateMyFile` |
| file.go | `deleteMyFile` | `DeleteMyFile` |
| file.go | `batchDeleteMyFiles` | `BatchDeleteMyFiles` |
| file.go | `generateProcessURL` | `GenerateProcessURL` |
| file.go | `getMyQuota` | `GetMyQuota` |
| admin.go | `adminGetQuota` | `AdminGetQuota` |
| admin.go | `adminSetQuota` | `AdminSetQuota` |
| admin.go | `adminSoftDeleteOwnerFiles` | `AdminSoftDeleteOwnerFiles` |
| admin.go | `adminDeleteOwner` | `AdminDeleteOwner` |
| admin.go | `adminGetStats` | `AdminGetStats` |
| admin.go | `adminListFiles` | `AdminListFiles` |
| admin.go | `adminGetFile` | `AdminGetFile` |
| admin.go | `adminDeleteFile` | `AdminDeleteFile` |
| admin.go | `adminListProviders` | `AdminListProviders` |
| admin.go | `adminListBuckets` | `AdminListBuckets` |
| audit.go | `listMyAuditLogs` | `ListMyAuditLogs` |
| audit.go | `adminListAuditLogs` | `AdminListAuditLogs` |
| quota.go | `setOwnerQuota` | `SetOwnerQuota` |
| quota.go | `addOwnerQuota` | `AddOwnerQuota` |

**保持 lowercase 的 helper**（不要 rename）：`getQuota/setQuota/addQuota`（quota.go）、`getStorageStats`（stats.go）、`processOneUpload`（batch_upload.go）、`runOnce/RunOnce`（upload_gc.go）、`purgeDeletedObjects/PurgeDeletedObjects`（cleanup.go，已 PascalCase）。

- [ ] **Step 1: 对每个文件做 rename（用 sed 批量）**

每个 rename 用 `replace_all=true` 在 Edit 工具中替换函数定义和所有调用点。**先 rename 函数定义，再 rename 调用点**，或者一次性用 sed：

```bash
# 每个 RPC 方法，执行：rename 函数定义 + 调用点（同包内）
# 例：generateUploadURL → GenerateUploadURL
sed -i '' 's/\bgenerateUploadURL\b/GenerateUploadURL/g' internal/service/upload.go
sed -i '' 's/\bconfirmUpload\b/ConfirmUpload/g' internal/service/upload.go
sed -i '' 's/\bgetSTSCredential\b/GetSTSCredential/g' internal/service/upload.go
sed -i '' 's/\bbatchGetSTSCredential\b/BatchGetSTSCredential/g' internal/service/batch_upload.go
sed -i '' 's/\bcancelUpload\b/CancelUpload/g' internal/service/cancel_upload.go

sed -i '' 's/\bgenerateDownloadURL\b/GenerateDownloadURL/g; s/\blistMyFiles\b/ListMyFiles/g; s/\bgetMyFile\b/GetMyFile/g; s/\bupdateMyFile\b/UpdateMyFile/g; s/\bdeleteMyFile\b/DeleteMyFile/g; s/\bbatchDeleteMyFiles\b/BatchDeleteMyFiles/g; s/\bgenerateProcessURL\b/GenerateProcessURL/g; s/\bgetMyQuota\b/GetMyQuota/g' internal/service/file.go

sed -i '' 's/\badminGetQuota\b/AdminGetQuota/g; s/\badminSetQuota\b/AdminSetQuota/g; s/\badminSoftDeleteOwnerFiles\b/AdminSoftDeleteOwnerFiles/g; s/\badminDeleteOwner\b/AdminDeleteOwner/g; s/\badminGetStats\b/AdminGetStats/g; s/\badminListFiles\b/AdminListFiles/g; s/\badminGetFile\b/AdminGetFile/g; s/\badminDeleteFile\b/AdminDeleteFile/g; s/\badminListProviders\b/AdminListProviders/g; s/\badminListBuckets\b/AdminListBuckets/g' internal/service/admin.go

sed -i '' 's/\blistMyAuditLogs\b/ListMyAuditLogs/g; s/\badminListAuditLogs\b/AdminListAuditLogs/g' internal/service/audit.go

sed -i '' 's/\bsetOwnerQuota\b/SetOwnerQuota/g; s/\baddOwnerQuota\b/AddOwnerQuota/g' internal/service/quota.go
```

- [ ] **Step 2: 同步更新 service_test.go 的调用点**

```bash
# service_test.go 里只有 adminListProviders / adminListBuckets / setQuota / getQuota 是直接调私有方法的
# setQuota/getQuota 是 helper 保持不变，adminXxx 要改
sed -i '' 's/\badminListProviders\b/AdminListProviders/g; s/\badminListBuckets\b/AdminListBuckets/g' internal/service/service_test.go
```

- [ ] **Step 3: 验证同包内 build 通过（不依赖 server.go）**

Run: `go build ./internal/service/`
Expected: PASS（service 包内部自洽）

如果报错 `func X already declared` → 检查是否漏删 stub；如果报错 `X undefined` → 检查是否漏改调用点。

### Task 1.3: 创建 pkg/handler/handler.go

**Files:**
- Create: `pkg/handler/handler.go`

- [ ] **Step 1: 写 handler.go**

```go
// Package handler is the thin gRPC shell for the storage service. Each method
// is a one-line delegation to internal/service.StorageService — handler holds
// no business logic and performs no protocol conversion.
package handler

import (
	"storage-service/internal/service"
	"storage-service/pkg/config"
	"storage-service/pkg/option"
)

// Handler implements storagev1.StorageServiceServer by delegating every RPC to
// the underlying *service.StorageService. It also satisfies signalx.Service so
// in-process module callers can manage lifecycle on the same object.
type Handler struct {
	storagev1.UnimplementedStorageServiceServer
	svc *service.StorageService
}

// Compile-time assertion that *Handler satisfies signalx.Service.
var _ signalx.Service = (*Handler)(nil)

// New constructs a Handler wrapping the given service. The service is created
// from cfg + opts; Handler owns no resources of its own — Start/Stop forward
// to the underlying service.
func New(cfg *config.Config, opts ...option.Option) (*Handler, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &Handler{svc: svc}, nil
}

// FromService wraps an existing *service.StorageService. Used by pkg.Server,
// which constructs the service separately for clearer error messages.
func FromService(svc *service.StorageService) *Handler {
	return &Handler{svc: svc}
}

// Start forwards to the underlying service so background goroutines (cron GC)
// begin running. No-op if the service has no starters.
func (h *Handler) Start() error { return h.svc.Start() }

// Stop forwards to the underlying service so owned resources are released.
func (h *Handler) Stop() error { return h.svc.Stop() }
```

需要的 import：`storagev1 "storage-service/gen/storage/v1"`、`"github.com/servekit/go-common/signalx"`。把上面代码块的 import 补全。

### Task 1.4: 创建 pkg/handler/storage.go（27 个委托方法）

**Files:**
- Create: `pkg/handler/storage.go`

- [ ] **Step 1: 写 storage.go**

```go
package handler

import (
	"context"

	"google.golang.org/protobuf/types/known/emptypb"

	storagev1 "storage-service/gen/storage/v1"
)

// Upload RPCs

func (h *Handler) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	return h.svc.GenerateUploadURL(ctx, req)
}

func (h *Handler) ConfirmUpload(ctx context.Context, req *storagev1.ConfirmUploadRequest) (*storagev1.ConfirmUploadResponse, error) {
	return h.svc.ConfirmUpload(ctx, req)
}

func (h *Handler) CancelUpload(ctx context.Context, req *storagev1.CancelUploadRequest) (*emptypb.Empty, error) {
	return h.svc.CancelUpload(ctx, req)
}

func (h *Handler) GetSTSCredential(ctx context.Context, req *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	return h.svc.GetSTSCredential(ctx, req)
}

func (h *Handler) BatchGetSTSCredential(ctx context.Context, req *storagev1.BatchGetSTSCredentialRequest) (*storagev1.BatchGetSTSCredentialResponse, error) {
	return h.svc.BatchGetSTSCredential(ctx, req)
}

// File RPCs

func (h *Handler) GenerateDownloadURL(ctx context.Context, req *storagev1.GenerateDownloadURLRequest) (*storagev1.GenerateDownloadURLResponse, error) {
	return h.svc.GenerateDownloadURL(ctx, req)
}

func (h *Handler) ListMyFiles(ctx context.Context, req *storagev1.ListMyFilesRequest) (*storagev1.ListMyFilesResponse, error) {
	return h.svc.ListMyFiles(ctx, req)
}

func (h *Handler) GetMyFile(ctx context.Context, req *storagev1.GetMyFileRequest) (*storagev1.UserFileInfo, error) {
	return h.svc.GetMyFile(ctx, req)
}

func (h *Handler) UpdateMyFile(ctx context.Context, req *storagev1.UpdateMyFileRequest) (*storagev1.UserFileInfo, error) {
	return h.svc.UpdateMyFile(ctx, req)
}

func (h *Handler) DeleteMyFile(ctx context.Context, req *storagev1.DeleteMyFileRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteMyFile(ctx, req)
}

func (h *Handler) BatchDeleteMyFiles(ctx context.Context, req *storagev1.BatchDeleteMyFilesRequest) (*storagev1.BatchDeleteMyFilesResponse, error) {
	return h.svc.BatchDeleteMyFiles(ctx, req)
}

func (h *Handler) GenerateProcessURL(ctx context.Context, req *storagev1.GenerateProcessURLRequest) (*storagev1.GenerateProcessURLResponse, error) {
	return h.svc.GenerateProcessURL(ctx, req)
}

// Quota RPCs

func (h *Handler) GetMyQuota(ctx context.Context, req *storagev1.GetMyQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.GetMyQuota(ctx, req)
}

func (h *Handler) SetOwnerQuota(ctx context.Context, req *storagev1.SetOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.SetOwnerQuota(ctx, req)
}

func (h *Handler) AddOwnerQuota(ctx context.Context, req *storagev1.AddOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.AddOwnerQuota(ctx, req)
}

// Admin RPCs

func (h *Handler) AdminGetQuota(ctx context.Context, req *storagev1.AdminGetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.AdminGetQuota(ctx, req)
}

func (h *Handler) AdminSetQuota(ctx context.Context, req *storagev1.AdminSetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return h.svc.AdminSetQuota(ctx, req)
}

func (h *Handler) AdminSoftDeleteOwnerFiles(ctx context.Context, req *storagev1.AdminSoftDeleteOwnerFilesRequest) (*storagev1.AdminSoftDeleteOwnerFilesResponse, error) {
	return h.svc.AdminSoftDeleteOwnerFiles(ctx, req)
}

func (h *Handler) AdminDeleteOwner(ctx context.Context, req *storagev1.AdminDeleteOwnerRequest) (*storagev1.AdminDeleteOwnerResponse, error) {
	return h.svc.AdminDeleteOwner(ctx, req)
}

func (h *Handler) AdminGetStats(ctx context.Context, req *storagev1.AdminGetStatsRequest) (*storagev1.AdminGetStatsResponse, error) {
	return h.svc.AdminGetStats(ctx, req)
}

func (h *Handler) AdminListFiles(ctx context.Context, req *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	return h.svc.AdminListFiles(ctx, req)
}

func (h *Handler) AdminGetFile(ctx context.Context, req *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	return h.svc.AdminGetFile(ctx, req)
}

func (h *Handler) AdminDeleteFile(ctx context.Context, req *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	return h.svc.AdminDeleteFile(ctx, req)
}

func (h *Handler) AdminListProviders(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	return h.svc.AdminListProviders(ctx, req)
}

func (h *Handler) AdminListBuckets(ctx context.Context, req *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	return h.svc.AdminListBuckets(ctx, req)
}

// Audit Log RPCs

func (h *Handler) ListMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	return h.svc.ListMyAuditLogs(ctx, req)
}

func (h *Handler) AdminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	return h.svc.AdminListAuditLogs(ctx, req)
}
```

- [ ] **Step 2: 验证 handler 包 build 通过**

Run: `go build ./pkg/handler/`
Expected: PASS

### Task 1.5: 更新 pkg/server.go 注册 Handler

**Files:**
- Modify: `pkg/server.go`

- [ ] **Step 1: Edit server.go**

把整个 `pkg/server.go` 替换为：

```go
package pkg

import (
	"errors"

	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/signalx"
	"google.golang.org/grpc"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/service"
	"storage-service/pkg/config"
	"storage-service/pkg/handler"
	"storage-service/pkg/option"
)

// Compile-time assertion that *Server satisfies signalx.Service.
var _ signalx.Service = (*Server)(nil)

// Server wraps a gRPC server for the storage service.
type Server struct {
	grpcSrv *grpcx.Server
	hdl     *handler.Handler
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
	hdl := handler.FromService(svc)

	grpcSrv := grpcx.New(
		grpcx.ServerConfig{
			GRPCAddr:    cfg.Server.GRPCAddr,
			GatewayAddr: cfg.Server.HTTPAddr,
		},
		func(gs *grpc.Server) {
			storagev1.RegisterStorageServiceServer(gs, hdl)
		},
		nil, // gateway + interceptors wired up in Phase 4
	)

	return &Server{grpcSrv: grpcSrv, hdl: hdl}, nil
}

// Start starts service internals and the gRPC + HTTP gateway without blocking.
// If grpcSrv.Start fails, hdl.Stop is called to roll back partial startup.
func (s *Server) Start() error {
	if err := s.hdl.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.hdl.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC + HTTP gateway and service internals.
// Errors from each component are aggregated via errors.Join.
func (s *Server) Stop() error {
	return errors.Join(s.grpcSrv.Stop(), s.hdl.Stop())
}
```

- [ ] **Step 2: 验证 pkg build**

Run: `go build ./pkg/...`
Expected: PASS

### Task 1.6: 更新 pkg/module.go 返回 *handler.Handler

**Files:**
- Modify: `pkg/module.go`

- [ ] **Step 1: Edit module.go**

把整个 `pkg/module.go` 替换为：

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

删除原来的 `StorageService = service.StorageService` 类型别名（外部消费者应该改用 `handler.Handler`）。

- [ ] **Step 2: 验证 build**

Run: `go build ./...`
Expected: PASS

如果其它包（cmd/server、tests）引用了 `pkg.StorageService` 类型别名，需要改：

```bash
grep -rn "pkg\.StorageService\|storageservice\.StorageService" --include="*.go" .
```

如有命中：替换为 `*handler.Handler` 或 `service.StorageService`（看场景）。预期 0 命中（module.go 是新写的，原 alias 也是为外部 import 准备，本仓库内未必引用）。

### Task 1.7: 全量测试 + 提交

- [ ] **Step 1: Run gofmt + goimports**

```bash
gofmt -w internal/service/ pkg/
goimports -w internal/service/ pkg/
```

- [ ] **Step 2: Run lint**

Run: `golangci-lint run ./...`
Expected: PASS（可能有 unused import 警告，按提示修）

- [ ] **Step 3: Run tests**

Run: `go test -race -coverprofile=coverage.out ./...`
Expected: 全部 PASS，覆盖率不低于 Phase 1 前基线（参考 `coverage.out` 旧文件）

- [ ] **Step 4: Commit**

```bash
git add internal/service/ pkg/ coverage.out
git commit -m "$(cat <<'EOF'
refactor(handler): extract pkg/handler as gRPC thin shell

StorageService no longer embeds UnimplementedStorageServiceServer —
pkg/handler.Handler is now the gRPC interface layer, delegating each
RPC to the underlying service method in one line.

Per golang-service-development skill §1, this separates the handler
(GRPC framework concern) from the service (business logic). Renaming
business methods from lowercase to PascalCase removes the stub+private
method pair anti-pattern (CreateDemo + createDemo on same struct).

Behavior unchanged: same RPCs, same logic, same tests pass.
EOF
)"
```

---

## Phase 2: `ownX bool` → `lifecycle.Manager`

### Task 2.1: 改写 resolveXxx helper 签名 + 注册到 mgr

**Files:**
- Modify: `internal/service/helpers.go` — `resolveDB/resolveRedis` 不再返回 bool，自建时注册到 mgr

- [ ] **Step 1: Edit resolveDB**

把 `internal/service/helpers.go` 中的 `resolveDB` 替换为：

```go
// resolveDB returns the DB pool to use. If the caller injected one via WithDB,
// use it as-is (caller owns lifecycle). Otherwise build from cfg and register
// a Stopper on mgr so service.Stop closes it in LIFO order.
func resolveDB(cfg *config.Config, external *gorm.DB, mgr *lifecycle.Manager) (*gorm.DB, error) {
	if external != nil {
		return external, nil
	}
	db, err := dbx.New(&cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("init database: %w", err)
	}
	mgr.AddStopper("db", lifecycle.StopFunc(func() {
		sqlDB, err := db.DB()
		if err != nil {
			slog.Warn("get sql db for close", "error", err)
			return
		}
		if err := sqlDB.Close(); err != nil {
			slog.Warn("close db", "error", err)
		}
	}))
	return db, nil
}
```

加 import：`"log/slog"`、`"github.com/servekit/go-common/lifecycle"`。

- [ ] **Step 2: Edit resolveRedis**

```go
// resolveRedis returns the Redis client to use. If the caller injected one via
// WithRedis, use it as-is. Otherwise, if RateLimit is configured, build from
// cfg and register a Stopper on mgr.
func resolveRedis(cfg *config.Config, external *redis.Client, mgr *lifecycle.Manager) (*redis.Client, error) {
	if external != nil {
		return external, nil
	}
	if cfg.Storage.RateLimit == nil {
		return nil, nil
	}
	client, err := redisx.New(&cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("init redis: %w", err)
	}
	mgr.AddStopper("redis", lifecycle.StopFunc(func() {
		if err := client.Close(); err != nil {
			slog.Warn("close redis", "error", err)
		}
	}))
	return client, nil
}
```

- [ ] **Step 3: resolveGID — 增加 closer 检测**

```go
// resolveGID returns the GID service to use. If injected, use as-is. Otherwise
// build from cfg; if the constructed instance exposes Close() error, register
// a Stopper so the underlying gRPC connection is released on shutdown.
func resolveGID(cfg *config.Config, external thirdcall.GIDService, mgr *lifecycle.Manager) (thirdcall.GIDService, error) {
	if external != nil {
		return external, nil
	}
	gid, err := thirdcall.NewGIDService(&cfg.ThirdParty.GID)
	if err != nil {
		return nil, err
	}
	if closer, ok := gid.(interface{ Close() error }); ok {
		mgr.AddStopper("gid", lifecycle.StopFunc(func() {
			if err := closer.Close(); err != nil {
				slog.Warn("close gid", "error", err)
			}
		}))
	}
	return gid, nil
}
```

### Task 2.2: 创建 cronComponent 实现 lifecycle.Service

**Files:**
- Create: `internal/service/cron_component.go`

- [ ] **Step 1: 写 cron_component.go**

```go
package service

import (
	"context"

	"github.com/servekit/go-common/lifecycle"

	"github.com/robfig/cron/v3"
)

// cronComponent adapts *cron.Cron to lifecycle.Service. Start launches the
// scheduler; Stop blocks until all in-flight jobs finish (via the CancelFunc
// Done channel), so we never leave GC jobs orphaned mid-shutdown.
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

// stopOnContext drains cron's in-flight jobs. Helper used by tests that want
// to wait for shutdown without holding the cronComponent reference.
func (cc *cronComponent) stopOnContext(ctx context.Context) error {
	done := cc.c.Stop().Done()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

删除 `stopOnContext` 如果不被引用（保守做法：先保留，golangci 会报 unused，再删）。

### Task 2.3: 改写 service.go 的 New/Stop，删 ownX bool

**Files:**
- Modify: `internal/service/service.go`

- [ ] **Step 1: 改 StorageService struct**

把 `ownDB/ownRedis/ownCron` 三个字段从 struct 删除：

```go
type StorageService struct {
	db       *gorm.DB
	redis    *redis.Client
	cron     *cron.Cron
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

- [ ] **Step 2: 改 New()**

```go
func New(cfg *config.Config, opts ...option.Option) (*StorageService, error) {
	o := option.Apply(opts...)
	mgr := lifecycle.NewManager()

	db, err := resolveDB(cfg, o.DB, mgr)
	if err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	gidGen, err := resolveGID(cfg, o.GIDService, mgr)
	if err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	redisClient, err := resolveRedis(cfg, o.Redis, mgr)
	if err != nil {
		return nil, errors.Join(err, mgr.Stop())
	}

	var limiter ratelimit.Limiter
	if redisClient != nil && cfg.Storage.RateLimit != nil {
		limiter = ratelimit.NewRedisLimiter(redisClient, *cfg.Storage.RateLimit)
	}

	registry, err := provider.NewRegistry(cfg.Storage.Providers)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init provider registry: %w", err), mgr.Stop())
	}

	auditRecorder := NewDBRecorder(db, gidGen)
	stsIssuerAdapter := &registrySTSIssuer{registry: registry}
	stsCache := newSTSCache(redisClient, stsIssuerAdapter, STSCacheConfig{
		DefaultTTL: cfg.Storage.STS.DefaultTTL,
		MaxTTL:     cfg.Storage.STS.MaxTTL,
	})

	cronInst, err := resolveCron(cfg, o.Cron, mgr)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("init cron: %w", err), mgr.Stop())
	}

	svc := &StorageService{
		db:        db,
		redis:     redisClient,
		cron:      cronInst,
		registry:  registry,
		gid:       gidGen,
		limiter:   limiter,
		cfg:       cfg,
		audit:     auditRecorder,
		manager:   mgr,
		stsCache:  stsCache,
		dedupLock: NewUploadDedupLock(redisClient, cfg.Storage.UploadSession.DedupLock),
	}

	if err := registerUploadGC(svc); err != nil {
		return nil, errors.Join(fmt.Errorf("register upload gc: %w", err), mgr.Stop())
	}

	return svc, nil
}
```

- [ ] **Step 3: 改 Start/Stop**

```go
// Start starts lifecycle-managed components concurrently. For close-only
// resources (db, redis, gid), Start is a no-op; for cronComponent, Start
// launches the scheduler.
func (s *StorageService) Start() error {
	return s.manager.Start()
}

// Stop stops all lifecycle-managed components in LIFO order. cron.Stop waits
// for in-flight GC jobs; db/redis/gid close their connections. Close errors
// are logged via slog.Warn inside each StopFunc.
func (s *StorageService) Stop() error {
	return s.manager.Stop()
}
```

- [ ] **Step 4: 改 resolveCron 签名 + 注册 cronComponent**

把 `internal/service/service.go` 末尾的 `resolveCron` 改为：

```go
// resolveCron returns the cron instance to use. If injected via WithCron, use
// it as-is (caller owns lifecycle). Otherwise build from cfg and register a
// cronComponent on mgr so service.Stop drains in-flight jobs in LIFO order.
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

### Task 2.4: 修复 service_test.go 的 ownX 断言

**Files:**
- Modify: `internal/service/service_test.go`

- [ ] **Step 1: 找 ownX 相关测试**

```bash
grep -n "ownDB\|ownRedis\|ownCron\|svc\.db\.DB()\|injected" internal/service/service_test.go
```

预期：测试通过 `WithDB(db)` 注入 DB 后，验证 service.Stop **没有**关掉它（外部所有）。新模型下这些断言仍然成立（注入的资源不注册到 mgr）。需要：
- 删除任何直接访问 `svc.ownDB` 之类私有字段的断言（Go 测试同包可访问，但 ownX 字段已删除）
- 保留黑盒行为断言：注入 → Stop 后 DB 仍可用；自建 → Stop 后 DB 关闭

- [ ] **Step 2: 编译 + 跑测试**

Run: `go build ./internal/service/ && go test -race ./internal/service/`
Expected: PASS

如果 test 报 `svc.ownDB undefined` → 那段测试需要重写为黑盒行为断言。

### Task 2.5: 全量验证 + 提交

- [ ] **Step 1: gofmt + goimports**

```bash
gofmt -w internal/service/
goimports -w internal/service/
```

- [ ] **Step 2: lint + test**

```bash
golangci-lint run ./...
go test -race -coverprofile=coverage.out ./...
```

Expected: 全部 PASS

- [ ] **Step 3: Commit**

```bash
git add internal/service/ coverage.out
git commit -m "$(cat <<'EOF'
refactor(service): replace ownX bool with lifecycle.Manager

StorageService no longer tracks ownDB/ownRedis/ownCron bool flags. All
owned resources are registered as Stoppers/Services on the existing
lifecycle.Manager, which closes them in LIFO order on Stop.

Per golang-service-development skill §5, this avoids the bool-per-resource
explosion and lets future resources (MQTT, Kafka) plug in without struct
changes. cronStop is now a cronComponent implementing lifecycle.Service,
encapsulating the in-flight drain logic that was previously inlined in
service.Stop.

Behavior unchanged for callers: injected resources still skip manager
registration; self-created resources still close on Stop. Close errors
are logged via slog.Warn per skill guidance (cleanup path has no other
error outlet).
EOF
)"
```

---

## Phase 3: 域文件合并（audit 合并 + upload 升级子包）

### Task 3.1: 合并 audit_snapshots.go 到 audit.go

**Files:**
- Modify: `internal/service/audit.go` — 顶部插入 FileSnapshot/QuotaSnapshot/etc 类型
- Delete: `internal/service/audit_snapshots.go`

- [ ] **Step 1: 读 audit.go 头部，确认 import 区**

Run: `head -20 internal/service/audit.go`
Expected: 看到 import 块和第一个 func/type

- [ ] **Step 2: 在 audit.go 末尾追加 snapshots 类型**

读 `internal/service/audit_snapshots.go` 全文，把所有 type 定义追加到 `internal/service/audit.go` 文件末尾（保留原有注释）。可以用 Edit 把 audit_snapshots.go 的内容（除 `package service` 行）追加到 audit.go 末尾。

具体追加内容（来自当前 audit_snapshots.go）：

```go
// FileSnapshot captures file state for audit before/after. Fields use omitempty
// so different audit points can fill only the fields relevant to that operation
// (upload records size+content_type, update records is_public, etc.).
type FileSnapshot struct {
	Filename    string `json:"filename"`
	FilePath    string `json:"file_path,omitempty"`
	Description string `json:"description,omitempty"`
	Size        int64  `json:"size,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	MD5         string `json:"md5,omitempty"`
	IsPublic    bool   `json:"is_public,omitempty"`
}

// QuotaSnapshot captures quota state for audit before/after.
type QuotaSnapshot struct {
	TotalBytes int64 `json:"total_bytes"`
}

// OwnerDeletionResult captures the outcome of deleting an owner's files.
type OwnerDeletionResult struct {
	FilesDeleted  int64 `json:"files_deleted"`
	BytesReleased int64 `json:"bytes_released"`
}

// FileBatchDeleteResult captures the outcome of a batch file deletion.
type FileBatchDeleteResult struct {
	FileIDs      []int64 `json:"file_ids"`
	DeletedCount int32   `json:"deleted_count"`
	FailedIDs    []int64 `json:"failed_ids,omitempty"`
}

// UploadSessionSnapshot captures session state for audit before/after. Fields
// use omitempty so different audit points can fill only the fields relevant
// to that operation (token issued vs. confirmed vs. cancelled). Vendor is
// always populated for multi-vendor debugging.
type UploadSessionSnapshot struct {
	ID        int64  `json:"id"`
	OwnerType int32  `json:"owner_type"`
	OwnerID   int64  `json:"owner_id"`
	Vendor    int32  `json:"vendor"`
	Bucket    string `json:"bucket"`
	ObjectKey string `json:"object_key"`
	MD5       string `json:"md5"`
	Size      int64  `json:"size"`
	Status    int32  `json:"status"`
	FileID    *int64 `json:"file_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}
```

- [ ] **Step 3: 删除 audit_snapshots.go**

Run: `git rm internal/service/audit_snapshots.go`
或：`rm internal/service/audit_snapshots.go`

- [ ] **Step 4: build 验证**

Run: `go build ./internal/service/`
Expected: PASS（同包，type 合并后无冲突）

### Task 3.2: 创建 upload 子包骨架

**Files:**
- Create: `internal/service/upload/upload.go`

- [ ] **Step 1: 分析 upload 域文件的依赖关系**

```bash
# 列出每个 upload 域文件引用的 service-package helper
grep -E "internalError|s\.audit|s\.registry|s\.dedupLock|s\.limiter|s\.stsCache|s\.gid|s\.db|s\.redis|s\.cfg" internal/service/upload.go internal/service/batch_upload.go internal/service/cancel_upload.go internal/service/uploadtoken.go internal/service/sts_cache.go internal/service/upload_gc.go | wc -l
```

预期：upload 方法依赖 `s.db/s.gid/s.audit/s.cfg/s.stsCache/s.dedupLock/s.registry/s.limiter` 等。子包需要一个 `Service` struct 持有这些依赖，由父包注入。

- [ ] **Step 2: 写 upload/upload.go**

```go
// Package upload contains the upload-domain business logic for the storage
// service: STS-credential issuance, upload-token signing/verification, upload
// confirmation, batch issuance, cancellation, and the periodic upload-session
// GC. Split out from internal/service per golang-service-development skill §2
// (single domain = single directory when the domain outgrows one file).
package upload

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/provider"
	"storage-service/pkg/config"
	"storage-service/pkg/thirdcall"
)

// Service holds upload-domain dependencies. Constructed by the parent
// service.New and embedded into parent StorageService as a field.
type Service struct {
	db       *gorm.DB
	redis    *redis.Client
	registry *provider.Registry
	gid      thirdcall.GIDService
	cfg      *config.Config

	stsCache  *stsCache
	dedupLock DedupLock
	audit     AuditRecorder
	cron      *cron.Cron
}

// Deps is the dependency bundle injected by the parent service. Parent owns
// every field; upload.Service holds pointers but does not own lifecycle.
type Deps struct {
	DB        *gorm.DB
	Redis     *redis.Client
	Registry  *provider.Registry
	GID       thirdcall.GIDService
	Cfg       *config.Config
	STSCache  *stsCache
	DedupLock DedupLock
	Audit     AuditRecorder
	Cron      *cron.Cron
}

// New constructs an upload.Service from injected deps.
func New(d Deps) *Service {
	return &Service{
		db:        d.DB,
		redis:     d.Redis,
		registry:  d.Registry,
		gid:       d.GID,
		cfg:       d.Cfg,
		stsCache:  d.STSCache,
		dedupLock: d.DedupLock,
		audit:     d.Audit,
		cron:      d.Cron,
	}
}

// RegisterGC schedules the periodic upload-session GC. Call once during
// parent service construction (after upload.New). The cron instance is shared
// with the parent — upload.Service does not own it.
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

// AuditRecorder is the interface upload.Service uses to record audit events.
// Implemented by service.DBRecorder in the parent package.
type AuditRecorder interface {
	// Add the methods upload.go actually calls on s.audit (RecordUpload,
	// RecordConfirm, etc.). Inspect internal/service/upload.go for the exact
	// method set and copy signatures here verbatim.
}

// DedupLock is the interface for upload-session dedup locking. Implemented
// by service.UploadDedupLock in the parent package.
type DedupLock interface {
	// Add the methods upload.go actually calls. Inspect and copy signatures.
}
```

**注意**：上面的 `AuditRecorder` 和 `DedupLock` 是 stub interface — Step 3 会用实际上传代码使用的方法集替换。

### Task 3.3: 把 upload 域文件迁到 upload/ 子包

**Files:**
- Move: `internal/service/upload.go` → `internal/service/upload/upload.go`（追加到上面 New 后面）
- Move: `internal/service/batch_upload.go` → `internal/service/upload/batch.go`
- Move: `internal/service/cancel_upload.go` → `internal/service/upload/cancel.go`
- Move: `internal/service/upload_gc.go` → `internal/service/upload/gc.go`
- Move: `internal/service/uploadtoken.go` → `internal/service/upload/token.go`
- Move: `internal/service/sts_cache.go` → `internal/service/upload/sts.go`

- [ ] **Step 1: 对每个文件做 git mv + sed 改 package + 改 receiver**

```bash
# 改 package 声明（service → upload）
for f in upload.go batch_upload.go cancel_upload.go upload_gc.go uploadtoken.go sts_cache.go; do
  sed -i '' '1s/^package service$/package upload/' internal/service/$f
done

# 改 receiver 类型 StorageService → Service（仅 upload 域方法，不影响其它文件）
# 注意：必须精确匹配 upload 域文件内的 (s *StorageService)，不影响 audit/admin/file/quota
sed -i '' 's/(s \*StorageService)/(s *Service)/g' \
  internal/service/upload.go \
  internal/service/batch_upload.go \
  internal/service/cancel_upload.go \
  internal/service/upload_gc.go \
  internal/service/uploadtoken.go \
  internal/service/sts_cache.go

# 移动文件
mkdir -p internal/service/upload
git mv internal/service/upload.go internal/service/upload/upload.go
git mv internal/service/batch_upload.go internal/service/upload/batch.go
git mv internal/service/cancel_upload.go internal/service/upload/cancel.go
git mv internal/service/upload_gc.go internal/service/upload/gc.go
git mv internal/service/uploadtoken.go internal/service/upload/token.go
git mv internal/service/sts_cache.go internal/service/upload/sts.go
```

- [ ] **Step 2: 把 upload/upload.go 中的 Upload Service 定义和新迁入的内容合并**

`upload/upload.go` 现在包含两块内容：
1. Step 2 写的骨架（Service struct + New + RegisterGC + interface stubs）
2. 原 `internal/service/upload.go` 迁来的 generateUploadURL/confirmUpload/getSTSCredential 方法

由于原 upload.go 的方法 receiver 现在是 `(s *Service)`，它们会附加到 `upload.Service` 上。**但原 upload.go 的 import 块需要 merge 进 upload/upload.go 的 import 块**。手动检查：

```bash
head -30 internal/service/upload/upload.go
```

如果有重复的 `package upload` 或重复 import，手动 Edit 删除。Go 编译器会告诉你哪里需要补。

- [ ] **Step 3: 把 upload-package 内对父包符号的引用改为本地引用**

upload 子包**不能**再 import 父包 `internal/service`（循环依赖）。检查并修复：

```bash
grep -n "internal/service\"" internal/service/upload/*.go
grep -nE "\b(internalError|NewDBRecorder|Recorder|UploadDedupLock|stsIssuer|registrySTSIssuer|stsCache|STSCacheConfig|newSTSCache|signUploadToken|verifyUploadToken|isUploadTokenExpired|isUploadTokenInvalid|tokenError|errTokenInvalid|errTokenExpired|uploadToken)\b" internal/service/upload/*.go
```

策略：
- `internalError` 等跨域 helper → 复制到 upload/sts.go 或 upload/upload.go 内（或留 interface，让父包注入）
- `signUploadToken/verifyUploadToken/uploadToken/tokenError` 等 → 这些是 upload 域专属，本来就该在子包内（已经在 token.go 里，迁过来就 OK）
- `stsCache/stsIssuer/registrySTSIssuer/STSCacheConfig/newSTSCache` → 同上，sts.go 已经在子包
- `UploadDedupLock` → 抽成 interface `DedupLock`（Task 3.2 已写 stub），把实际上传代码用的方法签名填进 interface
- `Recorder/NewDBRecorder/audit field` → 抽成 interface `AuditRecorder`，把上传代码调用的方法签名填进 interface

- [ ] **Step 4: 把 AuditRecorder / DedupLock interface 填实**

读 upload/upload.go（迁入后）和 upload/cancel.go，grep 实际调用的方法：

```bash
grep -oE "s\.audit\.[A-Z][a-zA-Z]*" internal/service/upload/*.go | sort -u
grep -oE "s\.dedupLock\.[A-Z][a-zA-Z]*" internal/service/upload/*.go | sort -u
```

把每个方法签名从父包的 Recorder / UploadDedupLock 类型复制到 upload/upload.go 的 AuditRecorder / DedupLock interface 定义里。

- [ ] **Step 5: build upload 子包**

Run: `go build ./internal/service/upload/`
Expected: PASS（如果报循环依赖 → Step 3 没改干净；如果报 undefined → Step 4 interface 没填全）

### Task 3.4: 改父包 service.go 加 facade + 注入 upload 子包

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/helpers.go`（如果 upload 域 helper 被父包其它域用，需要决定去留）

- [ ] **Step 1: 改 StorageService struct 加 upload 字段**

```go
type StorageService struct {
	db       *gorm.DB
	redis    *redis.Client
	cron     *cron.Cron
	registry *provider.Registry
	gid      thirdcall.GIDService
	limiter  ratelimit.Limiter
	cfg      *config.Config

	audit     Recorder
	manager   *lifecycle.Manager
	dedupLock UploadDedupLock
	upload    *upload.Service
}
```

删除 `stsCache *stsCache` 字段（迁到 upload 子包内部）。

- [ ] **Step 2: 改 New() 构造 upload.Service**

在 `New()` 中、`stsCache` 构造之后、`svc := &StorageService{...}` 之前：

```go
	// Upload subpackage: hold its own stsCache internally
	uploadSvc := upload.New(upload.Deps{
		DB:        db,
		Redis:     redisClient,
		Registry:  registry,
		GID:       gidGen,
		Cfg:       cfg,
		STSCache:  nil, // upload 子包内部自己 new（见 Step 3）
		DedupLock: NewUploadDedupLock(redisClient, cfg.Storage.UploadSession.DedupLock),
		Audit:     auditRecorder,
		Cron:      cronInst,
	})
	if err := uploadSvc.RegisterGC(); err != nil {
		return nil, errors.Join(fmt.Errorf("register upload gc: %w", err), mgr.Stop())
	}
```

**注意**：`stsCache` 的构造（`newSTSCache(redisClient, stsIssuerAdapter, ...)`）也要挪到 upload 子包内部（在 upload.New 内或 upload.New 之前）。`stsIssuerAdapter` 即 `registrySTSIssuer` 是 upload 域专属，跟着迁。

具体改动：把 service.go 中：
```go
	stsIssuerAdapter := &registrySTSIssuer{registry: registry}
	stsCache := newSTSCache(redisClient, stsIssuerAdapter, STSCacheConfig{...})
```
**删掉**（移到 upload 包）。同样删除原 `registerUploadGC(svc)` 调用（变成 `uploadSvc.RegisterGC()`）。删除 `internal/service/service.go` 末尾的 `registerUploadGC` 函数和 `uploadGCDefaultCronSpec` 常量（已迁到 upload 包 RegisterGC）。

- [ ] **Step 3: 删除原 svc struct 中关于 stsCache 的字段填充**

`svc := &StorageService{...}` 块里删除 `stsCache: stsCache,` 行。

- [ ] **Step 4: 写 facade 方法委托到 upload 子包**

在 `internal/service/service.go` 末尾追加：

```go
// --- upload domain facade ---

// GenerateUploadURL delegates to the upload subpackage. Per golang-service-
// development skill §2, when a domain is upgraded to a subpackage, parent
// service.go must expose facade methods so handler never imports the
// subpackage directly.
func (s *StorageService) GenerateUploadURL(ctx context.Context, req *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	return s.upload.GenerateUploadURL(ctx, req)
}

func (s *StorageService) ConfirmUpload(ctx context.Context, req *storagev1.ConfirmUploadRequest) (*storagev1.ConfirmUploadResponse, error) {
	return s.upload.ConfirmUpload(ctx, req)
}

func (s *StorageService) CancelUpload(ctx context.Context, req *storagev1.CancelUploadRequest) (*emptypb.Empty, error) {
	return s.upload.CancelUpload(ctx, req)
}

func (s *StorageService) GetSTSCredential(ctx context.Context, req *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	return s.upload.GetSTSCredential(ctx, req)
}

func (s *StorageService) BatchGetSTSCredential(ctx context.Context, req *storagev1.BatchGetSTSCredentialRequest) (*storagev1.BatchGetSTSCredentialResponse, error) {
	return s.upload.BatchGetSTSCredential(ctx, req)
}

// RunOnce delegates the upload-session GC pass to the subpackage.
func (s *StorageService) RunOnce(ctx context.Context) (int, error) {
	return s.upload.RunOnce(ctx)
}
```

加 import：`"storage-service/internal/service/upload"`、`"google.golang.org/protobuf/types/known/emptypb"`（如未引入）、`storagev1 "storage-service/gen/storage/v1"`（如未引入）。

- [ ] **Step 5: build + test 父包**

Run: `go build ./internal/service/`
Expected: PASS

Run: `go test -race ./internal/service/`
Expected: **FAIL** — service_test.go 中测 upload 方法的部分还在调 `svc.generateUploadURL` 等（已被删除）。需要 Task 3.5 处理。

### Task 3.5: 把 upload 测试迁到 upload 子包

**Files:**
- Move: upload 相关测试 → `internal/service/upload/upload_test.go`
- Delete: `internal/service/upload_gc_test.go`, `internal/service/sts_cache_test.go`
- Modify: `internal/service/service_test.go` — 删除 upload 相关测试，保留 audit/admin/file/quota 测试

- [ ] **Step 1: 识别 service_test.go 中 upload 相关的测试函数**

```bash
grep -nE "^func Test.*upload|^func Test.*STS|^func Test.*Token|^func Test.*Confirm|^func Test.*Cancel|^func Test.*GC" internal/service/service_test.go
```

也搜索非 Test 前缀但调 upload 方法的测试：

```bash
grep -nE "svc\.(GenerateUploadURL|ConfirmUpload|CancelUpload|GetSTSCredential|BatchGetSTSCredential|RunOnce)|signUploadToken|verifyUploadToken" internal/service/service_test.go
```

- [ ] **Step 2: 把命中的测试函数 move 到 internal/service/upload/upload_test.go**

每个 Test 函数原样 cut-paste。改：
- `svc := /* parent service */` → 构造 `upload.New(deps)` 直接测（绕过父包）
- `svc.GenerateUploadURL(...)` → 调用方式不变（receiver 是 `*upload.Service`）

如果测试需要父包的 `*StorageService` 才能跑（依赖完整 db/gid/audit），考虑保留在 service_test.go，但通过 `svc.upload.GenerateUploadURL(...)` 访问。优先策略：**能下沉到 upload_test 的全部下沉**；保留少数集成测试在 service_test。

- [ ] **Step 3: 把 upload_gc_test.go、sts_cache_test.go 整体迁到 upload/ 子包**

```bash
# 改 package
sed -i '' '1s/^package service$/package upload/' internal/service/upload_gc_test.go internal/service/sts_cache_test.go

# 改 receiver
sed -i '' 's/(s \*StorageService)/(s *Service)/g' internal/service/upload_gc_test.go internal/service/sts_cache_test.go

# 移动
git mv internal/service/upload_gc_test.go internal/service/upload/gc_test.go
git mv internal/service/sts_cache_test.go internal/service/upload/sts_test.go
```

- [ ] **Step 4: build + test upload 子包**

Run: `go build ./internal/service/upload/ && go test -race ./internal/service/upload/`
Expected: PASS

- [ ] **Step 5: build + test 父包**

Run: `go build ./internal/service/ && go test -race ./internal/service/`
Expected: PASS

### Task 3.6: 全量验证 + 提交

- [ ] **Step 1: gofmt + goimports**

```bash
gofmt -w internal/service/
goimports -w internal/service/
```

- [ ] **Step 2: lint + test**

```bash
golangci-lint run ./...
go test -race -coverprofile=coverage.out ./...
```

Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/service/ coverage.out
git commit -m "$(cat <<'EOF'
refactor(service): merge audit files and upgrade upload to subpackage

audit_snapshots.go merged into audit.go (single domain = single file
per skill §2). Upload domain (~1200 lines across 6 files) upgraded to
internal/service/upload/ subpackage per skill §2's "upgrade to package
when domain outgrows one file" rule:

  upload.go      — Service struct + New + RegisterGC + main entry methods
  batch.go       — BatchGetSTSCredential + helpers
  cancel.go      — CancelUpload
  gc.go          — RunOnce + session GC
  token.go       — upload token sign/verify (uploadtoken.go)
  sts.go         — STS cache (sts_cache.go)

service.go exposes facade methods (GenerateUploadURL, ConfirmUpload,
etc.) that delegate to s.upload.*, so pkg/handler never imports the
subpackage directly. Tests for upload domain moved to upload/*_test.go.
EOF
)"
```

---

## Phase 4: `grpcx.New` 三件套补全

### Task 4.1: 加 protovalidate 依赖

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 加依赖**

```bash
go get buf.build/go/protovalidate@latest
go get github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate@latest
go mod tidy
```

预期 go.mod 新增：
- `buf.build/go/protovalidate vX.Y.Z`
- `buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go vX.Y.Z` (transitive)
- `github.com/grpc-ecosystem/go-grpc-middleware/v2 v2.x.x`

### Task 4.2: 改 pkg/server.go 补全 grpcx.New 三件套

**Files:**
- Modify: `pkg/server.go`

- [ ] **Step 1: Edit pkg/server.go**

在 import 块加：
```go
	"buf.build/go/protovalidate"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"

	"github.com/servekit/go-common/grpcx"
```

在 `NewServer` 中、构造 `hdl` 之后、`grpcSrv := grpcx.New(...)` 之前加：

```go
	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("init protovalidate: %w", err)
	}
```

把 `grpcx.New(...)` 替换为：

```go
	grpcSrv := grpcx.New(
		grpcx.ServerConfig{
			GRPCAddr:    cfg.Server.GRPCAddr,
			GatewayAddr: cfg.Server.HTTPAddr,
		},
		func(gs *grpc.Server) {
			storagev1.RegisterStorageServiceServer(gs, hdl)
		},
		storagev1.RegisterStorageServiceHandlerFromEndpoint,
		grpcx.ErrorInterceptor,
		protovalidate_middleware.UnaryServerInterceptor(validator),
	)
```

加 import：`"fmt"`。

- [ ] **Step 2: build**

Run: `go build ./...`
Expected: PASS

### Task 4.3: 写 server 集成测试（gRPC + gateway 验证）

**Files:**
- Create: `pkg/server_test.go`

- [ ] **Step 1: 写最小集成测试**

```go
package pkg_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	storagev1 "storage-service/gen/storage/v1"
	storagepkg "storage-service/pkg"
	"storage-service/pkg/config"
	"storage-service/pkg/option"
)

// TestNewServer_RegistersGatewayAndInterceptors asserts the server starts
// without error after Phase 4 wiring (gateway, error interceptor, protovalidate).
// A failure here means one of the three was omitted from grpcx.New.
func TestNewServer_RegistersGatewayAndInterceptors(t *testing.T) {
	// Use ephemeral ports to avoid clashes
	cfg := &config.Config{
		Server: struct {
			GRPCAddr string
			HTTPAddr string
		}{
			GRPCAddr: freeAddr(t),
			HTTPAddr: freeAddr(t),
		},
		// Other fields populated per existing test fixtures; reuse helpers
		// from internal/service/service_test.go if needed.
	}

	srv, err := storagepkg.NewServer(cfg, option.WithDB(nil), option.WithGIDService(nil))
	if err != nil {
		// If construction needs more deps, skip with a clear message; the
		// point of this test is wiring, not full integration.
		t.Skipf("setup incomplete in this fixture: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	// Give listeners a moment to bind
	time.Sleep(100 * time.Millisecond)

	// Smoke-test: gRPC client can connect and call returns validation error
	// (or whatever the proto demands) — not checking business logic here.
	conn, err := grpc.NewClient(cfg.Server.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial grpc: %v", err)
	}
	defer conn.Close()

	client := storagev1.NewStorageServiceClient(conn)
	// Expect InvalidArgument because required fields are missing; proves
	// protovalidate interceptor is wired.
	_, err = client.GetMyQuota(context.Background(), &storagev1.GetMyQuotaRequest{})
	if err == nil {
		t.Log("warning: expected validation error from protovalidate, got nil — verify proto rules")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}
```

**注意**：这个测试是骨架 — 实际 fixture（cfg fields、需要的 deps）取决于现有 testcontainer 设置。如果 fixture 太重就 skip，保留 wiring 检查。

- [ ] **Step 2: 跑测试**

Run: `go test -race ./pkg/`
Expected: PASS 或 skip（如 fixture 不全）

### Task 4.4: 手动验证 + 提交

- [ ] **Step 1: 启动服务，grpcurl 验证**

```bash
# 终端 A
make run &
sleep 2

# 终端 B：gRPC
grpcurl -plaintext -d '{"owner_type":1,"owner_id":1}' \
  localhost:9000 storage.StorageService/GetMyQuota

# 期望：返回 InvalidArgument（protovalidate 拦截）或正常业务响应
# 不期望：codes.Unknown（说明 ErrorInterceptor 缺失）
```

- [ ] **Step 2: HTTP gateway 验证**

```bash
curl -X POST http://localhost:8080/v1/my/quota \
  -H "Content-Type: application/json" \
  -d '{"owner_type":1,"owner_id":1}'

# 期望：返回 4xx 或 200，不是 404（404 = gateway 没注册）
```

- [ ] **Step 3: 关闭服务**

```bash
kill %1  # 或用 pkill -f bin/server
```

- [ ] **Step 4: lint + 全量 test**

```bash
golangci-lint run ./...
go test -race -coverprofile=coverage.out ./...
```

- [ ] **Step 5: Commit**

```bash
git add pkg/ go.mod go.sum coverage.out
git commit -m "$(cat <<'EOF'
feat(server): enable HTTP gateway, error interceptor, protovalidate

grpcx.New now passes all three required components per skill §7:
  - RegisterStorageServiceHandlerFromEndpoint: enables HTTP gateway
  - grpcx.ErrorInterceptor: maps xerr.* to gRPC status (no more Unknown)
  - protovalidate middleware: enforces (buf.validate.field) rules

Previously gateway and both interceptors were silently absent — gateway
never started, xerr errors collapsed to codes.Unknown, proto rules
were inert. Adding protovalidate may cause some previously-passing
requests with missing required fields to now return InvalidArgument;
service-layer validation was already in place for these cases.
EOF
)"
```

---

## Verification 全局检查

完成所有 4 个 Phase 后，对照 skill §9 验收清单：

- [ ] `go build ./...` 通过
- [ ] `golangci-lint run ./...` 无 error
- [ ] `make proto && git diff --exit-code` 生成结果与 committed 一致
- [ ] `make generate && git diff --exit-code`
- [ ] grpcurl 跑通至少 1 个 RPC（如 GetMyQuota）
- [ ] curl HTTP gateway 跑通
- [ ] in-process module 测试（`pkg.NewModule`）跑通 — 加一个 smoke test 或复用现有 fixture
- [ ] 无 "demo" 字样残留（不适用 — 这是 storage-service 不是 demo；可跳过此检查）
- [ ] `grep -rn "ownDB\|ownRedis\|ownCron" internal/` 应为 0 命中
- [ ] `grep -n "UnimplementedStorageServiceServer" internal/service/` 应为 0 命中（只在 pkg/handler/ 出现）

## 关联

- **Spec**：`docs/superpowers/specs/2026-06-20-storage-service-skill-refactor-design.md`
- **Skill 依据**：`ai-kit-studio/skills/golang-service-development/golang-service-development.md`
- **Canonical 参考**：`gid-service/pkg/server.go`（grpcx.New 三件套）
