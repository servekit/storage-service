# File Pagination Design

- **Date**: 2026-06-23
- **Status**: Approved
- **Scope**: `internal/service/file/`, `internal/store/dal/`, `api/proto/storage/v1/storage.proto`
- **Out of scope**: `AdminListFiles`（独立改造，单独 PR）

## Background

当前 `ListMyFiles` 用 cursor 分页 + 每页全表 `COUNT(*)`：

- `service/file/file.go:106-191` 装配 `dbx.Pagination` + 解码 `PageCursor{id, filename, created_at}`
- `store/dal/file.go:103-152` `ListFilesByOwner` 每页跑一次 `q.Count(ctx, "*")`（L122），把结果塞进 `ListMyFilesResponse.total_count`（L188）

这是"cursor 复杂度 + count 开销"两头不讨好的组合：

- cursor 暴露脆弱性：c417625 修了排序键漏行的 bug，但只是补丁；admin 接口的同款 bug 还没修（见 Out of scope）
- UI 想跳页 / 显示页码导航，cursor 本质上做不到
- 每页都付一次全表 count，但用户视角的文件量本来就有限，count 也不省

## Goal

按职责拆分两个接口：

| RPC | 分页 | total_count | 用途 |
|-----|------|-------------|------|
| `ListMyFiles`（改造） | cursor | **去掉** | 后台扫描 / 批量遍历 / 导出 |
| `ListMyFilesPaged`（新增） | offset | **保留** | UI 列表 / 跳页导航 |

## Design

### 1. `ListMyFiles` 改造为纯 cursor

**proto** (`api/proto/storage/v1/storage.proto:485-489`)：

```proto
message ListMyFilesResponse {
  reserved 2;
  reserved "total_count";

  repeated File files = 1;
  string next_page_token = 3;
}
```

`reserved` 是 proto 最佳实践——防止未来有人复用编号 2 造成老客户端解析错误。

**service** (`internal/service/file/file.go:106-191`)：

- 不再装配 count，签名简化为返回 `(files, nextToken, error)`
- cursor 编解码保留 c417625 的 `pagination.PageCursor{id, filename, created_at}`

**DAL** (`internal/store/dal/file.go:103-152` `ListFilesByOwner`)：

- 删掉 L122 的 `q.Count(ctx, "*")`
- 返回值去掉 count 字段
- 排序与 cursor 比较逻辑保持不变

### 2. `ListMyFilesPaged` 新增 offset 分页接口

**proto**：

```proto
message ListMyFilesPagedRequest {
  int32 page = 1;       // 1-based
  int32 page_size = 2;
  string path_prefix = 3;
  string extension = 4;
  string content_type_prefix = 5;
  SortField order_by = 6;
  bool descending = 7;
}

message ListMyFilesPagedResponse {
  repeated File files = 1;
  int64 total_count = 2;
  int32 page = 3;
  int32 total_pages = 4;
  bool has_more = 5;
}

rpc ListMyFilesPaged(ListMyFilesPagedRequest) returns (ListMyFilesPagedResponse);
```

过滤/排序字段编号和语义跟 `ListMyFilesRequest` 对齐（方便后续迁移调用方）。

**service** (`internal/service/file/file.go`)：

- 新增 `ListMyFilesPaged(ctx, req)`
- 输入校验：
  - `page < 1` → `xcodes.ErrInvalidArgument`
  - `page_size < 1` 或 `page_size > MAX_PAGE_SIZE` → `xcodes.ErrInvalidArgument`（`MAX_PAGE_SIZE` 沿用现有 `ListMyFilesRequest.page_size` 上限，plan 阶段从 proto 注释 / service 校验代码确认具体值）
- 计算 `offset = (page - 1) * page_size`
- 调 DAL 拿 `(files, totalCount)`
- 计算 `total_pages = ceil(totalCount / page_size)`，`has_more = page < total_pages`

**DAL** (`internal/store/dal/file.go`)：

- 新增 `ListFilesByOwnerPaged(ctx, ownerID, ownerType, filter, page, pageSize) ([]*File, int64, error)`
- `LIMIT pageSize OFFSET (page-1)*pageSize`
- 一次 `COUNT(*)` 拿总数
- 排序走共享 helper（见下）

### 3. 共享排序逻辑提取

当前 `store/dal/file.go:127-140` 排序拼装在 cursor 路径里：

- `SORT_FIELD_FILENAME` → `filename, id`
- 其余（含 `SIZE`）→ `created_at, id`

提取私有 helper：

```go
// applyFileOrder applies the ordering clause shared by cursor and offset paths.
func applyFileOrder(q *gorm.DB, orderBy SortField, descending bool) *gorm.DB
```

两处调用（`ListFilesByOwner` cursor 版 + `ListFilesByOwnerPaged` offset 版）共用，避免实现漂移。

### 4. 错误处理

按 CLAUDE.md 规范统一走 `pkg/xcodes`：

- `page < 1` / `page_size` 越界 → `xcodes.ErrInvalidArgument.New()` 或带上下文消息的 `.New("page must be >= 1")`
- `page > total_pages` → **不报错**，返回空 `files` + 正确的 `total_pages` + `has_more=false`（这是前端跳过尾页的常规场景）
- 其他底层错误（DB 失败等）走 `xcodes.ErrInternal.Wrap(err)`

### 5. 数据流

**cursor 路径**（`ListMyFiles`）：

```
client (page_token)
  → service: decodePageCursor → (id, filename, created_at)
  → DAL.ListFilesByOwner (cursor compare, no COUNT)
  → service: encodePageCursor(last row) → next_page_token
  → response: files + next_page_token
```

**offset 路径**（`ListMyFilesPaged`）：

```
client (page, page_size)
  → service: validate, compute offset
  → DAL.ListFilesByOwnerPaged (OFFSET + LIMIT + COUNT)
  → service: compute total_pages, has_more
  → response: files + total_count + page + total_pages + has_more
```

## Testing

### 单元测试

`internal/service/file/file_test.go`：

- `ListMyFilesPaged` 输入校验：
  - `page=0` → `ErrInvalidArgument`
  - `page=-1` → `ErrInvalidArgument`
  - `page_size=0` → `ErrInvalidArgument`
  - `page_size > MAX` → `ErrInvalidArgument`
- `total_pages` 计算正确（含 `total=0` 时 `total_pages=0`）
- `has_more` 在 page=末页/超出时均为 false

### 集成测试（`dbx.SetupTestDB`）

`internal/store/dal/file_integration_test.go`：

- 插 25 条，`page_size=10`：
  - `page=1` → 10 条 + `total_count=25` + `total_pages=3` + `has_more=true`
  - `page=2` → 10 条 + `has_more=true`
  - `page=3` → 5 条 + `has_more=false`
  - `page=4` → 0 条 + `total_pages=3` + `has_more=false`
- 排序：按 `filename asc` 验证跨页顺序连续
- cursor 路径不再返回 count（断言 DAL 返回值结构变化）

### 现有 cursor 测试调整

`ListMyFiles` 现有的集成测试里：

- 删除对 `total_count` 的断言
- 验证响应**不含** `total_count`（防止后续误改回流）

## Migration / Compatibility

- proto 字段删除：用 `reserved` 保证 wire-level 兼容，老客户端解析新响应只是看不到 `total_count`（取 zero value 0）
- 现有 `ListMyFiles` 调用方需要适配：拿不到 `total_count` 了，如果有强依赖需要走 `ListMyFilesPaged`
- 新接口 `ListMyFilesPaged` 是纯新增，无破坏性

## Out of Scope

- `AdminListFiles`（`service/admin/admin.go:267-351`）改造：仍停在旧裸 id 游标，c417625 漏修了同款排序漏行 bug。本次不动，单独开 PR。
- 是否同时给 `AdminListFiles` 加 offset 版本：未来按需添加，本次不涉及。
- 上层 grpc-gateway / HTTP 路由是否暴露新 RPC：默认跟随 protoc 生成，无需额外改动。
