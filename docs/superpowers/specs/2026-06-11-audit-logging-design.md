# Audit Logging Design — storage-service

## 目标

为文件管理的写操作提供操作留痕能力。用户和管理员可通过 gRPC 接口查询操作记录，每条记录包含变更前后的详细 diff。

## 架构方案：事件收集器（Event Collector）

通过 `audit.Recorder` 接口在业务方法中记录操作事件，同步写入数据库。业务方法调用一行 `recorder.Record(ctx, event)` 即可，底层实现通过接口隔离。

选择此方案的原因：
- 需要详细的 before/after diff，gRPC 拦截器无法获取变更前的值和事务内的业务语义
- 通过接口注入，底层实现可替换（同步写库、异步队列等）
- 写操作方法数量有限（9 个），侵入性可控

## 数据模型

### `stor_audit_log` 表

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | int64 (snowflake) | 主键 |
| `action` | string | 操作类型枚举 |
| `owner_type` | int | 操作者类型（复用 proto OwnerType 枚举值） |
| `owner_id` | int64 | 操作者 ID |
| `target_type` | string | 对象类型：`file`、`quota`、`owner` |
| `target_id` | int64 | 操作对象 ID |
| `before` | JSONB | 变更前快照（可为 null） |
| `after` | JSONB | 变更后快照（可为 null） |
| `status` | string | 结果：`success` / `failed` |
| `error_message` | string | 失败原因（可为 null） |
| `request_id` | string | 请求 ID（用于追踪） |
| `created_at` | timestamp | 操作时间（非空，默认 now()） |

### 索引

- `(owner_type, owner_id, created_at DESC)` — 用户查自己的记录
- `(target_type, target_id, created_at DESC)` — 查某个对象的变更历史
- `(created_at DESC)` — 管理员查全部

不设外键约束（遵循项目约定），不用软删除（操作记录是不可变的审计数据）。

### 操作类型枚举

| action | 说明 |
|--------|------|
| `upload` | 文件上传（confirmUpload） |
| `update` | 文件信息更新 |
| `delete` | 单文件删除 |
| `batch_delete` | 批量删除 |
| `admin_delete` | 管理员删除 |
| `admin_set_quota` | 管理员设置配额 |
| `admin_soft_delete_owner` | 管理员软删除 Owner 文件 |
| `admin_delete_owner` | 管理员删除 Owner |

## 需要记录的写操作

| 方法 | action | target_type | before | after | 备注 |
|------|--------|-------------|--------|-------|------|
| `confirmUpload` | `upload` | `file` | nil | 新 File 信息 | 真正创建文件的时刻 |
| `updateMyFile` | `update` | `file` | 旧字段值 | 新字段值 | 只记录实际变更的字段 |
| `deleteMyFile` | `delete` | `file` | 旧 File 信息 | nil | |
| `batchDeleteMyFiles` | `batch_delete` | `file` | 文件列表 | nil | target_id 存第一个 ID，详细列表放 before |
| `adminDeleteFile` | `admin_delete` | `file` | 旧 File 信息 | nil | |
| `adminSetQuota` | `admin_set_quota` | `quota` | 旧配额 | 新配额 | |
| `adminSoftDeleteOwnerFiles` | `admin_soft_delete_owner` | `owner` | 文件数+字节数 | nil | |
| `adminDeleteOwner` | `admin_delete_owner` | `owner` | 文件数+配额 | nil | |

> `generateUploadURL` 不记录 — 它只是签名 URL，文件还没真正上传。`confirmUpload` 才是真正创建文件的时刻。

## 核心接口

```go
// internal/audit/event.go

type Event struct {
    Action     string         // 操作类型
    OwnerType  int32          // 操作者类型
    OwnerID    int64          // 操作者 ID
    TargetType string         // 对象类型: file/quota/owner
    TargetID   int64          // 对象 ID
    Before     map[string]any // 变更前字段（nil 表示新建）
    After      map[string]any // 变更后字段（nil 表示删除）
    Status     string         // success / failed
    Error      error          // 失败原因
}
```

```go
// internal/audit/recorder.go

type Recorder interface {
    Record(ctx context.Context, event Event) error
}
```

## 使用方式

以 `updateMyFile` 为例：

```go
func (s *StorageService) updateMyFile(ctx context.Context, req *storagev1.UpdateMyFileRequest) (*storagev1.UpdateMyFileResponse, error) {
    // 1. 查出旧值（before）
    oldFile, err := s.fileRepo.GetByIDAndOwner(ctx, req.GetId(), ...)

    // 2. 执行更新
    newFile, err := s.fileRepo.Update(ctx, ...)

    // 3. 记录操作
    s.audit.Record(ctx, audit.Event{
        Action:     "update",
        OwnerType:  ownerType,
        OwnerID:    ownerID,
        TargetType: "file",
        TargetID:   oldFile.ID,
        Before:     map[string]any{"filename": oldFile.Filename, "filepath": oldFile.FilePath},
        After:      map[string]any{"filename": newFile.Filename, "filepath": newFile.FilePath},
        Status:     "success",
    })
}
```

关键设计点：
1. `Record` 是同步调用，在业务事务提交后、返回响应前调用
2. Record 失败不阻塞业务返回（记录错误日志即可）
3. Before 值在业务操作前获取，After 值用业务操作结果
4. Recorder 通过构造函数注入，底层实现可替换

## 查询 API

### 用户接口

```protobuf
rpc ListMyAuditLogs(ListMyAuditLogsRequest) returns (ListMyAuditLogsResponse);
```

筛选条件：`action`、`target_type`、`start_time`/`end_time`

### 管理员接口

```protobuf
rpc AdminListAuditLogs(AdminListAuditLogsRequest) returns (AdminListAuditLogsResponse);
```

筛选条件：`action`、`target_type`、`status`、`request_id`、`owner_type`+`owner_id`、`target_id`、`start_time`/`end_time`

### 分页

游标分页，用 snowflake ID 作 page token，与现有 `ListMyFiles` 风格一致。

### 响应字段

操作类型、操作者信息（owner_type + owner_id）、对象信息（target_type + target_id）、before/after（JSON）、状态、错误信息、request_id、时间。

## 目录结构

```
internal/
├── audit/                    # 操作记录模块
│   ├── event.go              # Event 结构体定义
│   ├── recorder.go           # Recorder 接口 + DBRecorder 实现
│   └── nop_recorder.go       # 空实现（测试/开发用）
├── store/
│   ├── models/
│   │   └── audit_log.go      # AuditLog GORM 模型
│   └── repository/
│       └── audit_log_repo.go # 审计日志查询
└── service/
    └── upload.go / file.go / admin.go  # 现有文件，加 Record 调用
```

## 依赖注入

```go
// StorageService 新增字段
type StorageService struct {
    // ... 现有字段
    audit audit.Recorder
}

// 构造函数注入
func New(db *gorm.DB, ..., auditRecorder audit.Recorder, ...) *StorageService

// 开发/测试时可以注入 NopRecorder（什么都不做）
// 生产环境注入 DBRecorder
```

## 不引入新外部依赖

只用现有的 GORM + PostgreSQL，不引入消息队列或事件总线。

## 关联

- 上传速率限制设计：[[specs/2026-06-11-upload-rate-limiting-design]]
