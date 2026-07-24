# storage-service 全域子包化 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 audit / quota / file / admin 四个剩余域全部升级为子包，service.go 最终包含 27 个 facade 方法（与 pkg/handler/storage.go 1-to-1 对应）。

**Architecture:** 沿用 Phase 3 已建立的 upload/ 子包模式 —— 每个域有 Service struct + Deps 注入 + 可选 Host interface；父 StorageService 持有所有子包实例并实现它们的 Host interface。基础设施（audit Recorder、quota helper）作为最底层子包被业务域依赖。纯函数 utility 抽到 `internal/service/conv/` 子包消除循环依赖。

**Tech Stack:** Go 1.22+, `github.com/servekit/go-common`, GORM, PostgreSQL, 与 Phase 3 完全一致。

**Spec:** `docs/superpowers/specs/2026-06-20-domain-subpackage-extraction-design.md`

**Prior context:** Phase 3 已完成 upload/ 子包化，service.go 已有 6 个 upload facade。本计划是 Phase 3 模式的 ×4 复制。

---

## File Structure 总览

**新建子包**：
- `internal/service/conv/` — 纯函数 utility（ownerTypeToProto 等）
- `internal/service/audit/` — audit 域 + 基础设施（Recorder, Event, snapshot types）
- `internal/service/quota/` — quota 域 + helper
- `internal/service/file/` — file 域
- `internal/service/admin/` — admin 域 + stats + cleanup

**修改文件**：
- `internal/service/service.go` — 加 21 个新 facade + 改 New() 装配 + 加 audit/quota/file/admin 字段
- `internal/service/helpers.go` — 删 utility 函数（迁到 conv/），保留 resolveDB/GID/Redis
- `pkg/handler/storage.go` — **不变**（facade 方法名与原方法名一致）
- 各域 `*_test.go` — 迁入对应子包

**删除文件**（内容移走）：
- `internal/service/audit.go`
- `internal/service/file.go`
- `internal/service/quota.go`
- `internal/service/admin.go`
- `internal/service/stats.go`
- `internal/service/cleanup.go`

---

## Commit 0: conv 子包 setup（基础设施先行）

### Task 0.1: 创建 conv/ 子包

**Files:**
- Create: `internal/service/conv/conv.go`
- Modify: `internal/service/helpers.go` — 删除迁出的函数

- [ ] **Step 1: 写 conv/conv.go**

```go
// Package conv holds pure conversion helpers shared across service subpackages.
// All functions are stateless and depend only on the proto types — safe to call
// from any service subpackage without creating import cycles.
package conv

import (
	storagev1 "storage-service/gen/storage/v1"
	img "storage-service/internal/provider/imgproc"
)

// OwnerTypeToProto converts an int32 owner_type DB value to its proto enum.
func OwnerTypeToProto(t int32) storagev1.OwnerType {
	return storagev1.OwnerType(t)
}

// VendorToName maps a proto Vendor int32 to its enum name string (e.g. 2 →
// "VENDOR_AWS_S3"). Returns "" for VENDOR_UNSPECIFIED or unknown values.
func VendorToName(v int32) string {
	if storagev1.Vendor(v) == storagev1.Vendor_VENDOR_UNSPECIFIED {
		return ""
	}
	name, ok := storagev1.Vendor_name[v]
	if !ok {
		return ""
	}
	return name
}

// ACLToProto converts a string ACL key to its proto enum.
func ACLToProto(acl string) storagev1.BucketACL {
	switch acl {
	case "private":
		return storagev1.BucketACL_BUCKET_ACL_PRIVATE
	case "public_read":
		return storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ
	case "public_read_write":
		return storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ_WRITE
	default:
		return storagev1.BucketACL_BUCKET_ACL_UNSPECIFIED
	}
}

// ObjectKeyFromMD5 builds the storage object key from a prefix and MD5 hash.
// Format: {prefix}{md5[:2]}/{md5}
func ObjectKeyFromMD5(prefix, md5 string) string {
	if len(md5) < 2 {
		return prefix + md5
	}
	return prefix + md5[:2] + "/" + md5
}

// ResolveBucket returns the provided bucket name if non-empty, otherwise falls
// back to the configured default bucket.
func ResolveBucket(bucket, defaultBucket string) string {
	if bucket != "" {
		return bucket
	}
	return defaultBucket
}

// ProtoToImageOp converts a proto ImageProcessOp to an image.ImageOp.
func ProtoToImageOp(op *storagev1.ImageProcessOp) img.Op {
	if op == nil {
		return img.Op{}
	}

	var opType img.OpType
	switch op.GetType() {
	case storagev1.ImageProcessOp_TYPE_RESIZE:
		opType = img.OpResize
	case storagev1.ImageProcessOp_TYPE_CROP:
		opType = img.OpCrop
	case storagev1.ImageProcessOp_TYPE_QUALITY:
		opType = img.OpQuality
	case storagev1.ImageProcessOp_TYPE_FORMAT:
		opType = img.OpFormat
	case storagev1.ImageProcessOp_TYPE_WATERMARK:
		opType = img.OpWatermark
	case storagev1.ImageProcessOp_TYPE_ROTATE:
		opType = img.OpRotate
	default:
		opType = img.OpResize
	}

	return img.Op{
		Type:          opType,
		Width:         int(op.GetWidth()),
		Height:        int(op.GetHeight()),
		Format:        op.GetFormat(),
		Quality:       int(op.GetQuality()),
		ResizeMode:    op.GetResizeMode(),
		WatermarkText: op.GetWatermarkText(),
		RotateDegrees: int(op.GetRotateDegrees()),
	}
}
```

- [ ] **Step 2: 从 helpers.go 删除迁出的函数**

删除 helpers.go 中：`ownerTypeToProto`, `vendorToName`, `aclStringToProto`, `objectKeyFromMD5`, `resolveBucket`, `protoToImageOp`。保留 `buildUserFileInfo`, `buildAdminFileInfo`（这些会跟着 file/admin 子包走，但先留在 helpers.go，等对应子包 commit 时再迁）。

也删除 `import img "storage-service/internal/provider/imgproc"` 如果不再使用。

- [ ] **Step 3: 在 internal/service/ 各文件改用 conv.X**

```bash
# 找到所有使用旧 helper 名的地方
grep -rn "ownerTypeToProto\|vendorToName\|aclStringToProto\|objectKeyFromMD5\|resolveBucket\|protoToImageOp" --include="*.go" internal/service/ pkg/
```

对每处命中，替换为 `conv.X`（注意大小写）：
- `ownerTypeToProto(x)` → `conv.OwnerTypeToProto(x)`
- `vendorToName(x)` → `conv.VendorToName(x)`
- `aclStringToProto(x)` → `conv.ACLToProto(x)`
- `objectKeyFromMD5(p, m)` → `conv.ObjectKeyFromMD5(p, m)`
- `resolveBucket(b, d)` → `conv.ResolveBucket(b, d)`
- `protoToImageOp(x)` → `conv.ProtoToImageOp(x)`

涉及文件（grep 后确认实际清单，预期）：
- `internal/service/audit.go`
- `internal/service/file.go`
- `internal/service/admin.go`
- `internal/service/helpers.go`（buildUserFileInfo/buildAdminFileInfo 内部）
- `internal/service/upload/helpers.go`（如果有副本）
- `internal/service/upload/*.go`（如果有调用）

**注意 upload/helpers.go 里可能有 Phase 3 实施时复制的 utility 副本。如有，替换为 conv.X 调用并删除副本。**

每个文件加 `import "storage-service/internal/service/conv"`。

- [ ] **Step 4: build + test**

```bash
go build ./...
go test -race -count=1 ./internal/service/...
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/service/conv/ internal/service/
git commit -m "refactor(service): extract conv subpackage for shared converters"
```

---

## Commit 1: audit/ 子包

### Task 1.1: 创建 audit/ 子包骨架

**Files:**
- Create: `internal/service/audit/audit.go`

- [ ] **Step 1: 写 audit/audit.go 骨架**

```go
// Package audit implements the audit-logging domain for the storage service.
// It owns the Recorder (used by every other domain to record audit events),
// the snapshot types that capture before/after state, and the two RPCs that
// read audit logs.
package audit

import (
	"context"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/pkg/thirdcall"

	"gorm.io/gorm"
)

// Service holds audit-domain dependencies.
type Service struct {
	db  *gorm.DB
	gid thirdcall.GIDService
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB  *gorm.DB
	GID thirdcall.GIDService
}

// New constructs an audit.Service.
func New(d Deps) *Service {
	return &Service{db: d.DB, gid: d.GID}
}

// Recorder returns the audit recorder. Other subpackages accept this as a
// Deps field so they can record audit events without importing this package's
// internals.
func (s *Service) Recorder() Recorder {
	return NewDBRecorder(s.db, s.gid)
}
```

- [ ] **Step 2: 迁入类型定义**

把 `internal/service/audit.go` 中以下类型**复制并 export**（PascalCase）到 `audit/audit.go` 末尾：
- `Event` (struct) — 保持 exported
- `FileSnapshot`, `QuotaSnapshot`, `OwnerDeletionResult`, `FileBatchDeleteResult`, `UploadSessionSnapshot` — 保持 exported

注意：原 audit.go 里这些已经是 exported 类型，直接复制即可。

- [ ] **Step 3: 迁入 Recorder interface 和 DBRecorder impl**

把 `internal/service/audit.go` 中以下内容**移到** `audit/recorder.go`（新建）：
- `Recorder` interface
- `DBRecorder` struct + 所有方法
- `NewDBRecorder` 构造函数

所有方法 receiver 改为 `*DBRecorder`（如果不是已经这样）。原 `(s *StorageService) recordOutcome` 改为 `*DBRecorder` 上的方法。

### Task 1.2: 迁入 ListMyAuditLogs / AdminListAuditLogs

**Files:**
- Create: `internal/service/audit/list.go`
- Modify: `internal/service/audit/audit.go` — 可能需要 import

- [ ] **Step 1: 迁入 audit log RPC 方法**

把 `internal/service/audit.go` 中：
- `func (s *StorageService) ListMyAuditLogs(...)`
- `func (s *StorageService) AdminListAuditLogs(...)`

**移动**到 `internal/service/audit/list.go`，receiver 改为 `(s *Service)`。两个方法内部如果调用了 `recordOutcome`（不应该，它们是读操作），改为 `s.rec.RecordOutcome` 或类似。预期它们只用 `s.db`。

如果方法内部调用 helpers（如 `ownerTypeToProto`），改为 `conv.OwnerTypeToProto` 并 import conv。

### Task 1.3: 迁入 audit 测试

**Files:**
- Create: `internal/service/audit/audit_test.go`
- Modify: `internal/service/service_test.go` — 删除迁出的测试

- [ ] **Step 1: 识别 audit 相关测试**

```bash
grep -nE "^func Test.*[Aa]udit|svc\.(ListMyAuditLogs|AdminListAuditLogs)" internal/service/service_test.go
```

- [ ] **Step 2: 迁移测试函数**

把命中的 Test 函数 move 到 `audit/audit_test.go`。改：
- `package service` → `package audit`
- 调用方从父包构造改为 `audit.New(Deps{...})` 直接构造
- 调用方法从 `svc.X` 改为 `auditSvc.X`（receiver 已是 `*audit.Service`）

### Task 1.4: 删除 audit.go 并更新父包

**Files:**
- Delete: `internal/service/audit.go`
- Modify: `internal/service/service.go` — 加 audit 字段、构造、facade

- [ ] **Step 1: 删除 audit.go**

```bash
rm internal/service/audit.go
```

- [ ] **Step 2: 修改 StorageService struct**

在 service.go 的 `StorageService` struct 加 `audit *audit.Service` 字段：

```go
type StorageService struct {
	cfg      *config.Config
	manager  *lifecycle.Manager

	audit  *audit.Service
	upload *upload.Service
}
```

注意：原 struct 里的 `db`, `redis`, `cron`, `registry`, `gid`, `limiter` 等字段，**需要保留**，因为 file/admin/quota 子包需要这些资源（通过 Deps 注入）。或者重新评估：所有原 struct 字段都迁到子包，父 struct 只剩 `cfg`, `manager`, 5 个子包字段。**建议后者更干净**：

```go
type StorageService struct {
	cfg     *config.Config
	manager *lifecycle.Manager

	audit  *audit.Service
	quota  *quota.Service
	upload *upload.Service
	file   *file.Service
	admin  *admin.Service
}
```

但 New() 还需要构造各子包，所以在 New() 内部持有临时变量 db/gid/redis/registry/cron。

- [ ] **Step 3: 修改 New() 装配 audit**

在 New() 中、`mgr` 构造之后、`svc := &StorageService{...}` 之前，加：

```go
	// Audit first — quota/file/admin depend on its Recorder
	auditSvc := audit.New(audit.Deps{DB: db, GID: gidGen})
```

`svc` 构造时填 `audit: auditSvc`。

- [ ] **Step 4: 加 facade 方法**

在 service.go 末尾加：

```go
// --- audit domain facade ---

func (s *StorageService) ListMyAuditLogs(ctx context.Context, req *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	return s.audit.ListMyAuditLogs(ctx, req)
}

func (s *StorageService) AdminListAuditLogs(ctx context.Context, req *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	return s.audit.AdminListAuditLogs(ctx, req)
}
```

加 import `"storage-service/internal/service/audit"`。

- [ ] **Step 5: build + test**

```bash
go build ./...
go test -race -count=1 ./internal/service/...
```

Expected: PASS（service_test.go 中其它域测试可能因引用 audit.X 失败，临时改为通过 facade 调用或 import audit 包；本 commit 范围只动 audit 相关）

### Task 1.5: Commit

- [ ] **Step 1: gofmt + goimports**

```bash
gofmt -w internal/service/audit/ internal/service/
goimports -w internal/service/audit/ internal/service/
```

- [ ] **Step 2: lint + test**

```bash
golangci-lint run ./...
go test -race -count=1 ./...
```

- [ ] **Step 3: Commit**

```bash
git add internal/service/
git commit -m "$(cat <<'EOF'
refactor(service): extract audit subpackage

audit domain (Recorder, Event, snapshot types, DBRecorder, ListMyAuditLogs,
AdminListAuditLogs) moved to internal/service/audit/. Parent service.go adds
2 facade methods. Other domains will consume audit.Recorder via Deps injection
in subsequent commits.
EOF
)"
```

---

## Commit 2: quota/ 子包

### Task 2.1: 创建 quota/ 子包

**Files:**
- Create: `internal/service/quota/quota.go`

- [ ] **Step 1: 写骨架**

```go
// Package quota implements the quota-tracking domain: helper functions for
// checking/reserving/releasing quota, plus the SetOwnerQuota / AddOwnerQuota /
// GetMyQuota RPCs.
package quota

import (
	"context"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/service/audit"
	"storage-service/internal/store/models"

	"gorm.io/gorm"
)

// Service holds quota-domain dependencies.
type Service struct {
	db    *gorm.DB
	audit audit.Recorder
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB    *gorm.DB
	Audit audit.Recorder
}

// New constructs a quota.Service.
func New(d Deps) *Service {
	return &Service{db: d.DB, audit: d.Audit}
}
```

- [ ] **Step 2: 迁入 helpers 和 RPCs**

从 `internal/service/quota.go` 迁入：
- `func (s *StorageService) getQuota(ctx, db, ownerType, ownerID)` → `(s *Service) GetQuota`（导出，给其它包用）
- `func (s *StorageService) setQuota(ctx, db, ownerType, ownerID, totalBytes)` → `(s *Service) SetQuota`
- `func (s *StorageService) addQuota(ctx, db, ownerType, ownerID, delta)` → `(s *Service) AddQuota`
- `func (s *StorageService) SetOwnerQuota(ctx, req)` → `(s *Service) SetOwnerQuota`（已是 PascalCase）
- `func (s *StorageService) AddOwnerQuota(ctx, req)` → `(s *Service) AddOwnerQuota`

**关键**：原 `setQuota/addQuota/getQuota` 是 lowercase helper。子包内为了被 file/admin 注入调用，必须 export。改为 PascalCase。

从 `internal/service/file.go` 迁入：
- `func (s *StorageService) GetMyQuota(ctx, req)` → `internal/service/quota/quota.go`，receiver 改 `(s *Service)`

如果 SetOwnerQuota/AddOwnerQuota 内部调用 `s.recordOutcome`，改为 `s.audit.RecordOutcome`（Recorder 接口方法名）。注意原 `recordOutcome` 是 StorageService 方法，新的 audit.Recorder 接口需要导出这个方法。

### Task 2.2: 在 audit 包暴露 Recorder.RecordOutcome

**Files:**
- Modify: `internal/service/audit/recorder.go`

- [ ] **Step 1: 把 recordOutcome 加入 Recorder interface**

原 `Recorder` interface 可能没列 `RecordOutcome`。检查：

```bash
grep -A 20 "type Recorder interface" internal/service/audit/recorder.go
```

如果 `RecordOutcome` 不在 interface 里，加上：

```go
type Recorder interface {
	RecordOutcome(ctx context.Context, event Event, err error)
	RecordOutcomeInTx(ctx context.Context, tx *gorm.DB, event Event, err error)
	// ... 其它原方法
}
```

把 `(s *StorageService) recordOutcome` 和 `recordOutcomeInTx` 改为 `*DBRecorder` 的方法（PascalCase 导出版本）。

### Task 2.3: 迁入测试

- [ ] **Step 1: 识别 quota 测试**

```bash
grep -nE "^func Test.*[Qq]uota|svc\.(SetOwnerQuota|AddOwnerQuota|GetMyQuota)" internal/service/service_test.go
```

- [ ] **Step 2: 迁到 quota/quota_test.go**，receiver 改 `*quota.Service`。

### Task 2.4: 删除 quota.go 并更新父包

- [ ] **Step 1: 删除 quota.go**

```bash
rm internal/service/quota.go
```

- [ ] **Step 2: 父包 struct 加 quota 字段**

```go
type StorageService struct {
	// ...
	audit  *audit.Service
	quota  *quota.Service
	upload *upload.Service
}
```

- [ ] **Step 3: New() 装配**

在 audit 构造之后：

```go
	quotaSvc := quota.New(quota.Deps{DB: db, Audit: auditSvc.Recorder()})
```

- [ ] **Step 4: facade 方法**

```go
// --- quota domain facade ---

func (s *StorageService) GetMyQuota(ctx context.Context, req *storagev1.GetMyQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.quota.GetMyQuota(ctx, req)
}

func (s *StorageService) SetOwnerQuota(ctx context.Context, req *storagev1.SetOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.quota.SetOwnerQuota(ctx, req)
}

func (s *StorageService) AddOwnerQuota(ctx context.Context, req *storagev1.AddOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return s.quota.AddOwnerQuota(ctx, req)
}
```

- [ ] **Step 5: build + test + commit**

```bash
go build ./...
go test -race -count=1 ./...
gofmt -w internal/service/quota/ internal/service/
goimports -w internal/service/quota/ internal/service/
golangci-lint run ./...

git add internal/service/
git commit -m "refactor(service): extract quota subpackage"
```

---

## Commit 3: file/ 子包

### Task 3.1: 创建 file/ 子包

**Files:**
- Create: `internal/service/file/file.go`

- [ ] **Step 1: 骨架**

```go
// Package file implements the file-management domain: CRUD operations on
// user-owned files, plus URL generation for download and image processing.
package file

import (
	"context"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/provider"
	"storage-service/internal/service/audit"
	"storage-service/internal/service/quota"
	"storage-service/pkg/thirdcall"

	"gorm.io/gorm"
)

// Service holds file-domain dependencies.
type Service struct {
	db       *gorm.DB
	gid      thirdcall.GIDService
	registry *provider.Registry
	audit    audit.Recorder
	quota    *quota.Service
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB       *gorm.DB
	GID      thirdcall.GIDService
	Registry *provider.Registry
	Audit    audit.Recorder
	Quota    *quota.Service
}

// New constructs a file.Service.
func New(d Deps) *Service {
	return &Service{
		db: d.DB, gid: d.GID, registry: d.Registry,
		audit: d.Audit, quota: d.Quota,
	}
}
```

- [ ] **Step 2: 迁入 RPCs 和 helpers**

从 `internal/service/file.go` 迁入（receiver 改 `(s *Service)`）：
- `GenerateDownloadURL`
- `ListMyFiles`
- `GetMyFile`
- `UpdateMyFile`
- `DeleteMyFile`
- `BatchDeleteMyFiles`
- `GenerateProcessURL`

从 `internal/service/helpers.go` 迁入：
- `buildUserFileInfo`（file 域专属）

**注意**：`GetMyQuota` 已经在 Commit 2 迁到 quota/，不在 file/。

`recordOutcome` 调用改为 `s.audit.RecordOutcome`。
`getQuota` 等 helper 调用改为 `s.quota.GetQuota`。

### Task 3.2: 迁入测试

- [ ] **Step 1: 识别 file 测试**

```bash
grep -nE "^func Test.*(File|Download|Process)|svc\.(GenerateDownloadURL|ListMyFiles|GetMyFile|UpdateMyFile|DeleteMyFile|BatchDeleteMyFiles|GenerateProcessURL)" internal/service/service_test.go
```

- [ ] **Step 2: 迁到 file/file_test.go**

### Task 3.3: 删除 file.go 并更新父包

- [ ] **Step 1: 删除 file.go**

```bash
rm internal/service/file.go
```

- [ ] **Step 2: 父包 struct + New + facade**

加 `file *file.Service` 字段；New() 中：

```go
	fileSvc := file.New(file.Deps{
		DB: db, GID: gidGen, Registry: registry,
		Audit: auditSvc.Recorder(), Quota: quotaSvc,
	})
```

facade 方法 7 个：GenerateDownloadURL, ListMyFiles, GetMyFile, UpdateMyFile, DeleteMyFile, BatchDeleteMyFiles, GenerateProcessURL。

- [ ] **Step 3: build + test + commit**

Commit message: `refactor(service): extract file subpackage`

---

## Commit 4: admin/ 子包 + finalize

### Task 4.1: 创建 admin/ 子包

**Files:**
- Create: `internal/service/admin/admin.go`

- [ ] **Step 1: 骨架**

```go
// Package admin implements admin-facing operations: file management across
// owners, quota administration, stats aggregation, provider/bucket listing,
// and owner deletion with cascading cleanup.
package admin

import (
	"context"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/provider"
	"storage-service/internal/service/audit"
	"storage-service/internal/service/quota"
	"storage-service/pkg/thirdcall"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Service holds admin-domain dependencies.
type Service struct {
	db       *gorm.DB
	gid      thirdcall.GIDService
	registry *provider.Registry
	audit    audit.Recorder
	quota    *quota.Service
}

// Deps is the dependency bundle injected by the parent service.
type Deps struct {
	DB       *gorm.DB
	GID      thirdcall.GIDService
	Registry *provider.Registry
	Audit    audit.Recorder
	Quota    *quota.Service
}

// New constructs an admin.Service.
func New(d Deps) *Service {
	return &Service{
		db: d.DB, gid: d.GID, registry: d.Registry,
		audit: d.Audit, quota: d.Quota,
	}
}
```

- [ ] **Step 2: 迁入 RPCs（admin.go）**

10 个 RPC，receiver 改 `(s *Service)`：
- AdminListFiles, AdminGetFile, AdminDeleteFile
- AdminGetQuota, AdminSetQuota
- AdminGetStats
- AdminListProviders, AdminListBuckets
- AdminSoftDeleteOwnerFiles, AdminDeleteOwner

`buildAdminFileInfo` helper 也迁入（admin 域专属）。

- [ ] **Step 3: 迁入 stats.go 和 cleanup.go 内容**

- `getStorageStats` → admin 包内（export 为 `GetStorageStats` 如果跨包用；如仅 admin 用，保持 lowercase 也行，但为了一致性建议 export）
- `PurgeDeletedObjects`, `PurgeDeletedOwner`, `DeletedOwnerRetention` → admin 包内

### Task 4.2: 迁入测试

- [ ] **Step 1: 识别 admin 测试**

```bash
grep -nE "^func Test.*[Aa]dmin|svc\.Admin" internal/service/service_test.go
```

- [ ] **Step 2: 迁到 admin/admin_test.go**

### Task 4.3: 删除剩余文件并最终化父包

**Files:**
- Delete: `internal/service/admin.go`, `internal/service/stats.go`, `internal/service/cleanup.go`
- Modify: `internal/service/service.go`, `internal/service/helpers.go`
- Modify: `internal/service/service_test.go` — 剩余集成测试

- [ ] **Step 1: 删除 admin/stats/cleanup 文件**

```bash
rm internal/service/admin.go internal/service/stats.go internal/service/cleanup.go
```

- [ ] **Step 2: 检查 helpers.go 还剩什么**

```bash
cat internal/service/helpers.go
```

应该只剩：`resolveDB`, `resolveGID`, `resolveRedis`，以及可能的 `buildUserFileInfo`/`buildAdminFileInfo` 如果还没迁完。把这两个 build 函数迁到对应子包（file/admin）。helpers.go 最终只剩 resolve* 三个函数 + import。

- [ ] **Step 3: 父包 struct + New + 10 facade**

```go
type StorageService struct {
	cfg     *config.Config
	manager *lifecycle.Manager

	audit  *audit.Service
	quota  *quota.Service
	upload *upload.Service
	file   *file.Service
	admin  *admin.Service
}
```

New() 中加 admin 装配：

```go
	adminSvc := admin.New(admin.Deps{
		DB: db, GID: gidGen, Registry: registry,
		Audit: auditSvc.Recorder(), Quota: quotaSvc,
	})
```

加 10 个 admin facade 方法。

- [ ] **Step 4: 验证 service.go facade 计数**

```bash
grep -c "^func (s \*StorageService)" internal/service/service.go
# 期望 27（6 upload + 2 audit + 3 quota + 7 file + 10 admin - 1 因为 New 不算 - 实际数）
# 注意：原 New 之外，应该正好 27 个 RPC 方法（不含 Start/Stop/Registry/Host bridge 等非 RPC 方法）
```

精确统计 RPC facade：

```bash
grep -E "^func \(s \*StorageService\) (Generate|Confirm|Cancel|Get|Batch|List|Admin|Set|Add|Delete|Update|RunOnce)" internal/service/service.go | wc -l
# 期望: 6 (upload) + 2 (audit) + 3 (quota) + 7 (file) + 10 (admin) = 28
# 其中 RunOnce 是 upload GC 方法，不算 RPC（handler 不调），所以 RPC facade = 27
```

- [ ] **Step 5: 清理 service_test.go**

把剩余的跨域集成测试保留在 `service_test.go`。删除已迁到子包的测试。

`grep -c "^func Test" internal/service/service_test.go` 前后对比，确保测试总数守恒（迁移到子包的 + 留下的 = 原始）。

### Task 4.4: build + test + lint + commit

- [ ] **Step 1: 全量验证**

```bash
go build ./...
go test -race -count=1 ./...
gofmt -w internal/service/
goimports -w internal/service/
golangci-lint run ./...
```

- [ ] **Step 2: 验收检查**

```bash
# service.go 有 27 个 RPC facade
grep -cE "^func \(s \*StorageService\) (Generate|Confirm|Cancel|Get|Batch|List|Admin|Set|Add|Delete|Update)" internal/service/service.go
# 应为 27

# 原领域文件全部删除
ls internal/service/audit.go internal/service/file.go internal/service/quota.go internal/service/admin.go internal/service/stats.go internal/service/cleanup.go 2>&1
# 全部 "No such file"

# handler 不变
git diff main -- pkg/handler/storage.go  # 应为空或仅注释

# 子包存在
ls internal/service/{audit,quota,file,admin,upload,conv}/
```

- [ ] **Step 3: Commit**

```bash
git add internal/service/
git commit -m "$(cat <<'EOF'
refactor(service): extract admin subpackage and finalize 27-RPC facade

admin domain (10 admin RPCs + stats + cleanup helpers) moved to
internal/service/admin/. service.go now exposes all 27 RPCs as facade
methods, 1-to-1 with pkg/handler/storage.go.

Per the user's directive, service.go is now the single index of the
service's RPC surface — handler always calls svc.X, service.go always
delegates to a subpackage. The 6 pre-existing domains (audit, quota,
file, admin, upload) all follow the same subpackage pattern.

stats.go, cleanup.go merged into admin/ as domain helpers.
helpers.go trimmed to resolve* functions only; utility converters
moved to conv/ in Commit 0.
EOF
)"
```

---

## Verification 全局检查

完成后：

- [ ] `go build ./...` 通过
- [ ] `golangci-lint run ./...` 无新增 error
- [ ] `grep -cE "^func \(s \*StorageService\) (Generate|Confirm|Cancel|Get|Batch|List|Admin|Set|Add|Delete|Update)" internal/service/service.go` = 27
- [ ] `ls internal/service/` 输出：`service.go` `service_test.go` `helpers.go` `cron_component.go` + 6 个子包目录（audit/quota/file/admin/upload/conv）
- [ ] `git diff main -- pkg/handler/storage.go` 为空（handler 不变）
- [ ] 各子包 `go test ./...` 通过

## 关联

- **Spec**：`docs/superpowers/specs/2026-06-20-domain-subpackage-extraction-design.md`
- **前置**：原始 spec Phase 1-3 已完成
- **后续**：Phase 5（HTTP annotations）、Phase 6（grpcx.New）
- **参考**：`internal/service/upload/` 是子包化的 canonical 实现
