# Phase 5: HTTP Annotation 实施

> **For agentic workers:** Execute via subagent-driven-development. Single-commit phase.

**Goal:** 给 storage.proto 的 27 个 RPC 加上 `google.api.http` 注解，使 grpc-gateway 生成 `RegisterStorageServiceHandlerFromEndpoint`，让 HTTP gateway 真正可用。

**Architecture:** Google API Design Guide 风格。`/v1` 前缀。`My*` 端点的 Owner 走 query 参数（`owner.owner_type`/`owner.owner_id`），Admin 端点的 owner 走 path 参数。verb-style RPC 用 `:verb` 后缀。

**Tech Stack:** proto + google.api.http annotations + buf v2 + grpc-gateway。

**Prior context:** 用户在 Phase 4 blocker 时确认 Option B（先加注解再做 grpcx.New 接入）。`buf.yaml` 已有 `buf.build/googleapis/googleapis` 依赖；`buf.gen.yaml` 已有 `grpc-ecosystem/gateway` 插件。基础设施完备。

## 设计决策（已与用户对齐）

1. **URI 前缀**：`/v1`
2. **My* owner 获取**：query 参数（proto 的 Owner message 字段在 `?owner.owner_type=1&owner.owner_id=123`）
3. **Verb RPC**：Google `:verb` 后缀（如 `:batchDelete`、`:confirm`）
4. **路径参数**：file_id（int64）、owner_type（enum，path 里是 int）、owner_id（int64）

## 完整映射表

| # | RPC | Method | Path | Body |
|---|-----|--------|------|------|
| 1 | GenerateUploadURL | POST | `/v1/uploads` | `*` |
| 2 | ConfirmUpload | POST | `/v1/uploads:confirm` | `*` |
| 3 | CancelUpload | POST | `/v1/uploads:cancel` | `*` |
| 4 | GetSTSCredential | POST | `/v1/sts` | `*` |
| 5 | BatchGetSTSCredential | POST | `/v1/sts:batch` | `*` |
| 6 | ListMyFiles | GET | `/v1/files` | — |
| 7 | GetMyFile | GET | `/v1/files/{file_id}` | — |
| 8 | UpdateMyFile | PATCH | `/v1/files/{file_id}` | `*` |
| 9 | DeleteMyFile | DELETE | `/v1/files/{file_id}` | — |
| 10 | BatchDeleteMyFiles | POST | `/v1/files:batchDelete` | `*` |
| 11 | GenerateDownloadURL | POST | `/v1/files/{file_id}:generateDownloadUrl` | `*` |
| 12 | GenerateProcessURL | POST | `/v1/files/{file_id}:generateProcessUrl` | `*` |
| 13 | GetMyQuota | GET | `/v1/quota` | — |
| 14 | SetOwnerQuota | PUT | `/v1/owners/{owner_type}/{owner_id}/quota` | `total_bytes` |
| 15 | AddOwnerQuota | POST | `/v1/owners/{owner_type}/{owner_id}/quota:add` | `delta_bytes` |
| 16 | AdminListFiles | GET | `/v1/admin/files` | — |
| 17 | AdminGetFile | GET | `/v1/admin/files/{file_id}` | — |
| 18 | AdminDeleteFile | DELETE | `/v1/admin/files/{file_id}` | — |
| 19 | AdminGetQuota | GET | `/v1/admin/owners/{owner_type}/{owner_id}/quota` | — |
| 20 | AdminSetQuota | PUT | `/v1/admin/owners/{owner_type}/{owner_id}/quota` | `total_bytes` |
| 21 | AdminSoftDeleteOwnerFiles | POST | `/v1/admin/owners/{owner_type}/{owner_id}:softDeleteFiles` | `*` |
| 22 | AdminDeleteOwner | DELETE | `/v1/admin/owners/{owner_type}/{owner_id}` | — |
| 23 | AdminGetStats | GET | `/v1/admin/stats` | — |
| 24 | AdminListProviders | GET | `/v1/admin/providers` | — |
| 25 | AdminListBuckets | GET | `/v1/admin/buckets` | — |
| 26 | ListMyAuditLogs | GET | `/v1/audit-logs` | — |
| 27 | AdminListAuditLogs | GET | `/v1/admin/audit-logs` | — |

## Body 字段说明

- `body: "*"`：整个 request message 作为 HTTP body（POST/PUT/PATCH 用）。Path/query 中的字段仍从 path/query 提取，剩余字段从 body 反序列化。
- `body: "field_name"`：只把 message 中 `field_name` 字段映射到 body，其它字段从 path/query。
- 不写 body：所有字段从 path（如果有）和 query 参数提取（GET/DELETE 默认）。

## Tasks

### Task 5.1: 加 import + 27 个 RPC 注解

**Files:**
- Modify: `api/proto/storage/v1/storage.proto`

- [ ] **Step 1: 加 import**

在 storage.proto 现有 import 区（文件顶部 `import "buf/validate/validate.proto";` 之后）加：

```proto
import "google/api/annotations.proto";
```

- [ ] **Step 2: 给每个 RPC 加 `option (google.api.http)`**

按映射表给 service 块内每个 rpc 加 option。完整 service 块改造后形如：

```proto
service StorageService {
  rpc GenerateUploadURL(GenerateUploadURLRequest) returns (GenerateUploadURLResponse) {
    option (google.api.http) = {
      post: "/v1/uploads"
      body: "*"
    };
  }

  rpc GetSTSCredential(GetSTSCredentialRequest) returns (GetSTSCredentialResponse) {
    option (google.api.http) = {
      post: "/v1/sts"
      body: "*"
    };
  }

  rpc BatchGetSTSCredential(BatchGetSTSCredentialRequest) returns (BatchGetSTSCredentialResponse) {
    option (google.api.http) = {
      post: "/v1/sts:batch"
      body: "*"
    };
  }

  rpc ConfirmUpload(ConfirmUploadRequest) returns (ConfirmUploadResponse) {
    option (google.api.http) = {
      post: "/v1/uploads:confirm"
      body: "*"
    };
  }

  rpc CancelUpload(CancelUploadRequest) returns (google.protobuf.Empty) {
    option (google.api.http) = {
      post: "/v1/uploads:cancel"
      body: "*"
    };
  }

  rpc GenerateDownloadURL(GenerateDownloadURLRequest) returns (GenerateDownloadURLResponse) {
    option (google.api.http) = {
      post: "/v1/files/{file_id}:generateDownloadUrl"
      body: "*"
    };
  }

  rpc ListMyFiles(ListMyFilesRequest) returns (ListMyFilesResponse) {
    option (google.api.http) = { get: "/v1/files" };
  }

  rpc GetMyFile(GetMyFileRequest) returns (UserFileInfo) {
    option (google.api.http) = { get: "/v1/files/{file_id}" };
  }

  rpc UpdateMyFile(UpdateMyFileRequest) returns (UserFileInfo) {
    option (google.api.http) = {
      patch: "/v1/files/{file_id}"
      body: "*"
    };
  }

  rpc DeleteMyFile(DeleteMyFileRequest) returns (google.protobuf.Empty) {
    option (google.api.http) = { delete: "/v1/files/{file_id}" };
  }

  rpc BatchDeleteMyFiles(BatchDeleteMyFilesRequest) returns (BatchDeleteMyFilesResponse) {
    option (google.api.http) = {
      post: "/v1/files:batchDelete"
      body: "*"
    };
  }

  rpc GenerateProcessURL(GenerateProcessURLRequest) returns (GenerateProcessURLResponse) {
    option (google.api.http) = {
      post: "/v1/files/{file_id}:generateProcessUrl"
      body: "*"
    };
  }

  rpc GetMyQuota(GetMyQuotaRequest) returns (QuotaInfo) {
    option (google.api.http) = { get: "/v1/quota" };
  }

  rpc AdminListFiles(AdminListFilesRequest) returns (AdminListFilesResponse) {
    option (google.api.http) = { get: "/v1/admin/files" };
  }

  rpc AdminGetFile(AdminGetFileRequest) returns (AdminFileInfo) {
    option (google.api.http) = { get: "/v1/admin/files/{file_id}" };
  }

  rpc AdminDeleteFile(AdminDeleteFileRequest) returns (google.protobuf.Empty) {
    option (google.api.http) = { delete: "/v1/admin/files/{file_id}" };
  }

  rpc AdminGetQuota(AdminGetQuotaRequest) returns (QuotaInfo) {
    option (google.api.http) = { get: "/v1/admin/owners/{owner_type}/{owner_id}/quota" };
  }

  rpc AdminSetQuota(AdminSetQuotaRequest) returns (QuotaInfo) {
    option (google.api.http) = {
      put: "/v1/admin/owners/{owner_type}/{owner_id}/quota"
      body: "total_bytes"
    };
  }

  rpc AdminGetStats(AdminGetStatsRequest) returns (AdminGetStatsResponse) {
    option (google.api.http) = { get: "/v1/admin/stats" };
  }

  rpc AdminListProviders(google.protobuf.Empty) returns (AdminListProvidersResponse) {
    option (google.api.http) = { get: "/v1/admin/providers" };
  }

  rpc AdminListBuckets(google.protobuf.Empty) returns (AdminListBucketsResponse) {
    option (google.api.http) = { get: "/v1/admin/buckets" };
  }

  rpc AdminSoftDeleteOwnerFiles(AdminSoftDeleteOwnerFilesRequest) returns (AdminSoftDeleteOwnerFilesResponse) {
    option (google.api.http) = {
      post: "/v1/admin/owners/{owner_type}/{owner_id}:softDeleteFiles"
      body: "*"
    };
  }

  rpc AdminDeleteOwner(AdminDeleteOwnerRequest) returns (AdminDeleteOwnerResponse) {
    option (google.api.http) = { delete: "/v1/admin/owners/{owner_type}/{owner_id}" };
  }

  rpc ListMyAuditLogs(ListMyAuditLogsRequest) returns (ListMyAuditLogsResponse) {
    option (google.api.http) = { get: "/v1/audit-logs" };
  }

  rpc AdminListAuditLogs(AdminListAuditLogsRequest) returns (AdminListAuditLogsResponse) {
    option (google.api.http) = { get: "/v1/admin/audit-logs" };
  }

  rpc SetOwnerQuota(SetOwnerQuotaRequest) returns (QuotaInfo) {
    option (google.api.http) = {
      put: "/v1/owners/{owner_type}/{owner_id}/quota"
      body: "total_bytes"
    };
  }

  rpc AddOwnerQuota(AddOwnerQuotaRequest) returns (QuotaInfo) {
    option (google.api.http) = {
      post: "/v1/owners/{owner_type}/{owner_id}/quota:add"
      body: "delta_bytes"
    };
  }
}
```

**注意 path 参数字段名**：
- `file_id` 必须是 request message 的直接字段（GetMyFileRequest.file_id、AdminGetFileRequest.file_id 等都是 ✓）
- `owner_type` / `owner_id` 必须是直接字段（AdminGetQuotaRequest.owner_type、AdminGetQuotaRequest.owner_id、SetOwnerQuotaRequest.owner_type、SetOwnerQuotaRequest.owner_id、AddOwnerQuotaRequest.owner_type、AddOwnerQuotaRequest.owner_id 等都是 ✓）

### Task 5.2: 重新生成 + 验证

- [ ] **Step 1: buf generate**

```bash
make proto
```

期望：`gen/storage/v1/storage.pb.gw.go` 生成（之前没有这个文件）。

- [ ] **Step 2: 验证 gateway 函数存在**

```bash
grep "func RegisterStorageServiceHandlerFromEndpoint" gen/storage/v1/*.pb.gw.go
# 应该有命中
```

- [ ] **Step 3: 验证 build**

```bash
go build ./...
```

期望：通过。

### Task 5.3: 提交

- [ ] **Step 1: 提交**

```bash
git add api/proto/storage/v1/storage.proto gen/
git commit -m "$(cat <<'EOF'
feat(proto): add google.api.http annotations for grpc-gateway

Adds HTTP REST mappings to all 27 storage RPCs. URI scheme follows
Google API Design Guide:
  - /v1 prefix
  - My* endpoints read owner from query (?owner.owner_type=N&owner.owner_id=N)
  - Admin endpoints use owner as path param (/v1/admin/owners/{type}/{id}/...)
  - Verb-style RPCs use :verb suffix (e.g. /v1/files:batchDelete)

buf generate now produces gen/storage/v1/storage.pb.gw.go with
RegisterStorageServiceHandlerFromEndpoint, enabling the HTTP gateway
that was previously a no-op (grpcx.New was passing nil for gateway
registration).

Phase 6 will wire the gateway into pkg/server.go.
EOF
)"
```

## 风险

1. **path 参数类型**：grpc-gateway 默认支持 int64 / string / enum。owner_type 是 enum（int32）—— grpc-gateway 会接受 int 字面量（如 `/v1/admin/owners/1/123/quota`）。验证一下生成的代码确实处理 enum path param。
2. **body: "field_name"** 形式：service 期望整个 request 反序列化。`body: "total_bytes"` 意味着 HTTP body 只有一个 int64 值。如果客户端习惯发 JSON `{"total_bytes": 1000}`，要用 `body: "*"`。**重要**：检查 service 实现，决定哪种 body 形式更友好。如果不确定，**统一用 `body: "*"`** 更安全（接受 JSON 整个 request）。

**Task 5.1 Step 2 的修正**：把所有 `body: "total_bytes"` 和 `body: "delta_bytes"` 改为 `body: "*"`。理由：客户端发 JSON 更直观，且 `body: "field_name"` 形式 grpc-gateway 期望 body 是裸值（如 `1000`），不符合 REST 习惯。

## 验收

- [ ] `make proto && git diff --exit-code` 生成结果与 committed 一致
- [ ] `go build ./...` 通过
- [ ] `grep "RegisterStorageServiceHandlerFromEndpoint" gen/storage/v1/*.pb.gw.go` 有命中
- [ ] 单 commit

## 关联

- **前置 spec**：`docs/superpowers/specs/2026-06-20-storage-service-skill-refactor-design.md`
- **后续**：Phase 6（grpcx.New 五参数）
