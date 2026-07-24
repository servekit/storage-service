# Upload Session 设计:取 token 时建上传记录

日期:2026-06-16
分支:feat/sts-token(从 feat/audit-logging 切出)
关联:[[2026-06-16 STS Token 实现:Aliyun OSS 与 AWS S3|sts-token-implementation-design]]

## 背景与动机

当前 `GetSTSCredential` / `GenerateUploadURL` 流程是无状态的:

1. 取 token:checkQuota + 限流 + MD5 去重 + 签 HMAC token,**完全不写 DB**
2. 客户端直传到 OSS
3. `ConfirmUpload`:验签 + HeadObject 反查 OSS → 创建 StorageObject + File + reserve quota

**问题**:
- 客户端上传了但没 ConfirmUpload → OSS 留下孤儿对象,无法定位、清理
- 没法看"进行中的上传",审计断档
- ConfirmUpload 失败后(网络抖动)只能等 token 过期再重头来

**目标**:把上传改成 stateful 的两阶段:
1. 取 token 时**建 UploadSession 记录**(status=PENDING)+ 签 token(内含 session_id)
2. 客户端直传 OSS
3. `ConfirmUpload` 验签 + lookup session + HeadObject + 事务:创建 File、reserve quota、session 转 CONFIRMED
4. 过期/取消的 session:GC 任务清理 OSS 孤儿

## 当前状态

| 组件 | 现状 | 改动 |
|---|---|---|
| `models.UploadSession` | 不存在 | 新增 model |
| `models.StorageObject` | `(md5, size)` 全局唯一 + `Provider string` | 改 `(vendor, bucket, md5)` 唯一,`Provider` 改 `Vendor int32` |
| `service.uploadToken` | HMAC 携带 metadata | 加 `SessionID int64` 字段 |
| `service.getSTSCredential` | 只签 token | 取/缓存 STS 凭证(用户维度)+ DB 查重 + 加锁 + 建 session + 签 token |
| `service.batchGetSTSCredential` | 不存在 | 新增,内部循环单文件逻辑 |
| `service.confirmUpload` | 验签 + HeadObject + 建记录 | 验签 + lookup session + 校验 status + HeadObject + 事务建 File + reserve quota + session CONFIRMED |
| `service.generateUploadURL` | 与 STS 同结构 | 同步改造(presigned URL 路径) |
| `service.RunOnce` (GC) | 不存在 | 新增,扫过期 PENDING 清 OSS 孤儿,被 cron 调度或 admin RPC 复用 |
| `option.WithCron` | 不存在 | 新增 option,跟 db/redis 一致:不传则从 config 创建 cronx 实例 |
| proto | ConfirmUpload 入参只有 upload_token | **不变**(session_id 嵌在 token 里)。GetSTSCredential 加 `ttl` 字段。BatchGetSTSCredential 新增 |
| `service/quota.go` | check/reserve/release | **不改动**(只动 UsedBytes) |
| 错误码 | ErrUploadTokenInvalid/Expired | 新增 ErrUploadSessionNotFound / ErrUploadSessionNotPending / ErrUploadSessionExpired |
| STS 凭证 | 每次调用 STS API | Redis 缓存,用户维度共享 |

## 设计

### 1. UploadSession model

```go
// internal/store/models/upload_session.go
type UploadSession struct {
    ID          int64      `gorm:"primaryKey"`
    OwnerType   int32      `gorm:"column:owner_type;type:smallint;not null;default:1"`
    OwnerID     int64      `gorm:"column:owner_id;not null;index:idx_upload_sessions_owner,condition:deleted_at IS NULL"`
    Bucket      string     `gorm:"column:bucket;type:varchar(128);not null"`
    ObjectKey   string     `gorm:"column:object_key;type:varchar(512);not null"`
    MD5         string     `gorm:"column:md5;type:varchar(32);not null"`
    Size        int64      `gorm:"column:size;not null"`
    ContentType string     `gorm:"column:content_type;type:varchar(128);not null"`
    Filename    string     `gorm:"column:filename;type:varchar(256);not null"`
    FilePath    string     `gorm:"column:file_path;type:varchar(512)"`
    Description string     `gorm:"column:description;type:text"`
    Metadata    MapJSON    `gorm:"column:metadata;type:jsonb"`
    IsPublic    bool       `gorm:"column:is_public;not null;default:false"`
    Vendor      int32      `gorm:"column:vendor;type:smallint;not null"`

    Status      int32      `gorm:"column:status;type:smallint;not null;default:0;index:idx_upload_sessions_status_expires,priority:1,condition:deleted_at IS NULL"`
    FileID      *int64     `gorm:"column:file_id;index:idx_upload_sessions_file_id"`
    ExpiresAt   time.Time  `gorm:"column:expires_at;not null;index:idx_upload_sessions_status_expires,priority:2,condition:deleted_at IS NULL"`

    CreatedAt   time.Time  `gorm:"column:created_at;not null;autoCreateTime"`
    UpdatedAt   time.Time  `gorm:"column:updated_at;not null;autoUpdateTime"`
}
```

**Status 枚举**(proto 中定义,映射 int32):

```proto
enum UploadSessionStatus {
  UPLOAD_SESSION_STATUS_UNSPECIFIED = 0;
  UPLOAD_SESSION_STATUS_PENDING     = 1;
  UPLOAD_SESSION_STATUS_CONFIRMED   = 2;
  UPLOAD_SESSION_STATUS_EXPIRED     = 3;
  UPLOAD_SESSION_STATUS_CANCELLED   = 4;
}
```

**索引设计**:
- `(status, expires_at)` 复合索引:GC 扫 PENDING + 过期的高效
- `file_id` 反查:已知 file 反查 session(审计用)
- `(owner_type, owner_id)`:列出某 owner 的上传历史
- 全部带 `condition:deleted_at IS NULL`(预留软删除)

### 2. StorageObject 改造

把当前的 `(md5, size)` 全局唯一约束改成 `(vendor, bucket, md5)` 唯一,允许跨 vendor / 跨 bucket 的同 md5 共存(物理上不同存储位置)。`Provider string` 改 `Vendor int32`(对齐 commit 73e0161 的 RPC 层重命名)。

```go
// internal/store/models/object.go
type StorageObject struct {
    ID           int64      `gorm:"primaryKey"`
    Vendor       int32      `gorm:"column:vendor;type:smallint;not null;uniqueIndex:idx_storage_objects_vendor_bucket_md5,condition:deleted_at IS NULL"`
    Bucket       string     `gorm:"column:bucket;type:varchar(128);not null;uniqueIndex:idx_storage_objects_vendor_bucket_md5,condition:deleted_at IS NULL;uniqueIndex:idx_storage_objects_bucket_key_active,condition:deleted_at IS NULL"`
    ObjectKey    string     `gorm:"column:object_key;type:varchar(512);not null;uniqueIndex:idx_storage_objects_bucket_key_active,condition:deleted_at IS NULL"`
    MD5          string     `gorm:"column:md5;type:varchar(32);not null;uniqueIndex:idx_storage_objects_vendor_bucket_md5,condition:deleted_at IS NULL"`
    Size         int64      `gorm:"column:size;not null"`
    // ... 其他字段不变
}
```

**Size 不再参与唯一约束**。MD5 碰撞的兜底由应用层 size 二次校验保证(ConfirmUpload 时 HeadObject 返回的 size 跟 session.size 比对)。

**迁移**:AutoMigrate 时 drop 旧索引 `idx_storage_objects_md5_size` + 建新索引 `idx_storage_objects_vendor_bucket_md5`。当前无数据,无需迁移历史数据。

**应用层去重查询**:
```sql
SELECT * FROM storage_objects
WHERE vendor = ? AND bucket = ? AND md5 = ? AND deleted_at IS NULL
LIMIT 1;
```

### 3. Quota 模型 —— 不预占,只 check

**不引入 ReservedBytes,不加新接口**。

| 流程 | Quota 操作 |
|---|---|
| GetSTSCredential | `checkQuota(used + size <= total)` —— 只读 |
| ConfirmUpload | `reserve(used += size)` —— 沿用现有逻辑,带 check |
| CancelUpload | **不动** |
| GC | **不动** |

**session 的价值**(没有 reserve 后为什么仍要建 session):
1. GC 能精准清理 OSS 孤儿(`object_key` pin 在 session 里)
2. 审计有"取了 token"这件事的记录
3. ConfirmUpload 幂等(同 token 命中 CONFIRMED session 直接返回,不重复建 file)
4. 进行中的上传可见(admin 能看 PENDING 列表)

**取舍**:用户狂拿 token 不传 → quota 不爆(DB 不动),OSS 会有孤儿但 GC 兜底。reserve 的复杂度(并发、回滚、约束)不值得。

### 4. STS 凭证缓存

STS 凭证按 `(owner_type, owner_id, vendor, bucket)` 维度在 Redis 共享。同用户同 bucket 在 TTL 内只调一次 STS API。

**Redis 结构**:
```
key:    sts:cache:<owner_type>:<owner_id>:<vendor>:<bucket>
value:  {access_key_id, access_key_secret, security_token, expiration}
TTL:    调用方传的 TTL(在 max_ttl 上限内)
```

**TTL 由调用方传**:
- `GetSTSCredentialRequest.ttl` 字段
- 服务端校验:0 → 用 default_ttl; > max_ttl → 截断为 max_ttl
- 业务方根据场景传(管理员给长 TTL 是合理的,后台批量上传;普通用户短 TTL)
- 同用户不会传不同 TTL,直接用调用方传的 TTL 作 cache TTL

**取凭证流程**(singleflight 模式):
```
1. GET sts:cache:<owner>:<vendor>:<bucket>
   命中 → 直接返回
   未命中 → 进入 step 2
2. Acquire Redis 锁 sts:lock:<owner>:<vendor>:<bucket>
   拿到锁 → 双重检查 GET → 调 STS API → SET Redis(带 TTL) → Release 锁
   拿不到锁 → 短暂 backoff → 重试 step 1
```

**为什么按用户而不是全局共享**:
- 安全:不同用户的凭证互相隔离,泄漏半径缩小到单用户
- 仍大幅减少 STS 调用(每个用户每个 TTL 内一次)

**policy 范围**:STS policy 放宽到 `acs:oss:::bucket/<prefix>/*`,因为 object_key = `<prefix>/<md5>` 是确定性函数,跟用户无关。凭证对 prefix 下所有 md5 都有 PutObject 权限是设计上的必然。

**降级**:Redis 缓存只是优化。STS API 挂了 → 缓存空 → 服务返回 5xx。可接受。

### 5. uploadToken 扩展

```go
type uploadToken struct {
    SessionID int64  `json:"sid"`  // 新增
    // ... 其他字段保留
}
```

token 仍承担"防篡改"职责。session 是真实状态来源,token 内字段(session_id、md5、size、owner)用于在 ConfirmUpload 时**交叉校验 session**(防止 session 被替换)。

### 6. GetSTSCredential 流程

```
1. checkUploadRateLimit
2. MD5 去重(SELECT storage_objects WHERE vendor, bucket, md5)
   命中 → 返回 instant File(无 session,无 STS)
3. 解析 bucket、bucketCfg、provider、vendor
4. 取 STS 凭证(从 Redis 缓存或调 API,见 §4)
5. checkQuota(used + size <= total)  ← 只读,不动 DB
6. session 去重(见 §11):
   - DB 查重同 (owner, md5, size) 的 PENDING session
   - 加 Redis 锁
   - 命中 → 复用 session_id
   - 未命中 → 建 UploadSession{status=PENDING, object_key=<prefix>/<md5>, expires_at=now+ttl}
7. 签 token(含 session_id)
8. 返回 {upload_token, STS creds, expires_at}
```

**注意**:步骤 4(STS 缓存)和步骤 6(session 创建)互相独立。STS 失败不影响 session(理论上)。但实际实现中,建议先取 STS(可能耗时)再建 session,避免 STS 失败时留下孤儿 session。

### 7. BatchGetSTSCredential 流程

单次请求 N 个文件,共享同用户的 STS 凭证,各自建 session。

```proto
rpc BatchGetSTSCredential(BatchGetSTSCredentialRequest) returns (BatchGetSTSCredentialResponse);

message BatchGetSTSCredentialRequest {
  repeated UploadFileMeta files = 1;  // {md5, size, content_type, filename, ...}
  string bucket = 2;
  google.protobuf.Duration ttl = 254;
  Owner owner = 255;
  string request_id = 256;
}

message BatchGetSTSCredentialResponse {
  STSCredential sts_credential = 1;       // 共享,只一份
  repeated UploadCredentialItem items = 2; // 顺序对应 request.files
}

message UploadCredentialItem {
  oneof result {
    UploadTokenInfo token = 1;  // {upload_token, expires_at, file_id?}
    ItemError error = 2;        // {code, message}
  }
}
```

**流程**:
```
1. 校验 len(files) <= config.batch_max_size
2. checkUploadRateLimit × N(防止用批量绕过限流)
3. 取 STS 凭证(一次,共享)
4. gorx.NewTaskRunner(config.batch_concurrency) 并发处理每个 file:
   - MD5 去重 → 命中返回 instant File
   - checkQuota(used + size_i <= total)
   - DB 查重 + 加锁 → 创建/复用 session
   - 签 token
5. 返回 {sts_credential, items[]}
```

**部分失败**:`items[i]` 对应 `files[i]`,失败的 item 返回 `ItemError`,整体 RPC 不报错。客户端按需重试。

### 8. ConfirmUpload 流程

```
1. verifyUploadToken(token) → 拿到 session_id、md5、size、owner
2. lookup UploadSession by session_id
   - 不存在 → ErrUploadSessionNotFound
   - 已 CONFIRMED → 幂等返回 {file_id, file_info}(拿 session.file_id 反查)
   - EXPIRED/CANCELLED → ErrUploadSessionExpired
   - PENDING → 继续
3. 交叉校验:token.{owner, md5, size} == session.{...}
4. HeadObject 反查 OSS,验证存在 + MD5/size 对齐
5. 事务:
   - StorageObject CreateOrGet(同当前逻辑)
   - Create File(file_id 写回 session)
   - reserve quota(used += size)  ← 沿用现有接口,带 check
   - UPDATE session SET status=CONFIRMED, file_id=...
6. 返回 {file_id, file_info}
```

**幂等性**:重复 ConfirmUpload(同 token)→ 步骤 2 命中 CONFIRMED 分支,直接返回,**不重复扣 quota、不重复建 file**。

### 9. CancelUpload RPC(可选)

```proto
rpc CancelUpload(CancelUploadRequest) returns (google.protobuf.Empty);

message CancelUploadRequest {
  string upload_token = 1;
  Owner owner = 255;
  string request_id = 256;
}
```

行为:
- 验签 + lookup session
- 必须是 PENDING 状态(否则 ErrUploadSessionNotPending)
- 事务:UPDATE session SET status=CANCELLED(**不释放 quota**,因为没扣过)
- 不删 OSS 对象(可能客户端正在传,删了会有竞态;交给 GC)

不要这个 RPC 也行 —— GC 兜底。建议保留,客户端显式取消更友好。

### 10. GC 清理

```go
// internal/service/upload_gc.go

// RunOnce 扫一批过期 PENDING session,清理 OSS 孤儿。
// 纯逻辑,可被 cron 调度或 admin RPC 复用。
func (s *StorageService) RunOnce(ctx context.Context) error {
    // 1. SELECT * FROM upload_sessions
    //    WHERE status=PENDING AND expires_at < NOW() AND deleted_at IS NULL
    //    LIMIT batch_size
    // 2. 对每条:
    //    a. HeadObject(session.bucket, session.object_key)
    //       - 不存在 → 客户端没传成功,直接转 EXPIRED
    //       - 存在 → 孤儿!记 audit log + DeleteObject + 转 EXPIRED
    //    b. UPDATE session SET status=EXPIRED
    // 3. 不动 quota(没扣过)
}
```

**调度方式:cron 实例可注入(跟 db/redis 一致)**

不在服务里硬编码创建 cron,而是通过 `pkg/option` 注入。调用方不传则服务自己从 config 创建,跟现有 `WithDB` / `WithRedis` / `WithGIDService` 模式完全对齐。

```go
// pkg/option/option.go
type Options struct {
    DB         *gorm.DB
    Redis      *redis.Client
    GIDService thirdcall.GIDService
    Cron       *cronx.Cron  // 新增
}

func WithCron(c *cronx.Cron) Option {
    return func(o *Options) { o.Cron = c }
}
```

**服务初始化逻辑**(`pkg/module.go` 或 `pkg/server.go`):

```go
cron := opts.Cron
if cron == nil {
    cron = cronx.New(&cronx.Config{
        Timezone:      cfg.Storage.Cron.Timezone,
        OverlapPolicy: "skip",
    })
}

// 注册 GC 周期任务
cron.AddFunc(cfg.Storage.UploadGC.CronSpec, func() {
    if err := svc.RunOnce(ctx); err != nil {
        slog.Error("upload gc", "err", err)
    }
})
cron.Start()
// 服务 Stop 时:cron.Stop()
```

**好处**:
- 跟 db/redis 一致的注入模式,使用习惯统一
- in-process 调用方可以共享已有 cron 实例(其他任务已经创建过)
- `RunOnce` 仍是纯逻辑,单测 + admin RPC 复用

### 11. Session 去重(DB + Redis 锁)

GetSTSCredential 时,先 DB 查重,有同 (owner, md5, size) 的 PENDING 未过期 session 就复用 session_id。并发用 Redis 锁防止"同时查都查不到,各自建一条"。

**DB 查重 SQL**:
```sql
SELECT * FROM upload_sessions
WHERE owner_type = ? AND owner_id = ? AND md5 = ? AND size = ?
  AND status = PENDING AND expires_at > NOW() AND deleted_at IS NULL
ORDER BY id DESC LIMIT 1;
```

**Redis 锁**(用 go-common/redisx.NewLock):
```go
lock := redisx.NewLock(redisClient, &redisx.LockConfig{
    Prefix: cfg.Storage.UploadSession.DedupLock.Prefix,  // 默认 "upload:dedup"
    TTL:    cfg.Storage.UploadSession.DedupLock.TTL,     // 默认 10s
    Tries:  cfg.Storage.UploadSession.DedupLock.Tries,   // 默认 3
    Wait:   cfg.Storage.UploadSession.DedupLock.Wait,    // 默认 100ms
})

target := fmt.Sprintf("%d:%d:%s:%d", ownerType, ownerID, md5, size)
id, err := lock.Acquire(ctx, target)
if err != nil {
    // 拿不到锁:不阻塞,直接新建 session(GC 兜底)
    return createNewSession(...)
}
defer lock.Release(ctx, target, id)

session := findPendingSession(...)  // 双重检查
if session != nil {
    return session  // 复用
}
return createNewSession(...)
```

**锁不强制**:拿不到锁继续走新建路径。最坏情况两个并发请求各自建一条 session,object_key 相同(`<prefix>/<md5>`),GC 清理一条。

### 12. 审计事件 + Snapshot

新增审计动作:

```proto
AUDIT_ACTION_UPLOAD_SESSION_CREATE    // 取 token
AUDIT_ACTION_UPLOAD_SESSION_CONFIRM   // confirm 成功(等价于现有 AUDIT_ACTION_UPLOAD)
AUDIT_ACTION_UPLOAD_SESSION_CANCEL    // 显式取消
AUDIT_ACTION_UPLOAD_SESSION_GC        // GC 清理
```

`AUDIT_ACTION_UPLOAD`(现有)保留,ConfirmUpload 主路径继续用它,新的 SESSION_CREATE/CANCEL 是补充。

**新增 `UploadSessionSnapshot` struct**(参考 commit 33e428c 的类型化 snapshot 模式):

```go
// internal/service/audit.go(或类似)
type UploadSessionSnapshot struct {
    ID          int64
    OwnerType   int32
    OwnerID     int64
    Bucket      string
    ObjectKey   string
    MD5         string
    Size        int64
    Status      int32
    FileID      *int64
    ExpiresAt   time.Time
}
```

- `SESSION_CREATE`:Before=nil,After=`UploadSessionSnapshot{Status=PENDING}`
- `SESSION_CONFIRM`:Before=PENDING snapshot,After=CONFIRMED snapshot(FileID 填上)
- `SESSION_CANCEL` / `SESSION_GC`:类似

### 13. 测试策略

| 测试 | 类型 |
|---|---|
| `TestGetSTSCredential_CreatesSession` | 集成(postgres + mock STS) |
| `TestGetSTSCredential_DedupReuseSession` | 集成(同 owner+md5+size 复用 session) |
| `TestGetSTSCredential_STSCache` | 集成(同用户两次只调一次 STS) |
| `TestBatchGetSTSCredential_PartialFailure` | 集成 |
| `TestBatchGetSTSCredential_Concurrency` | 集成(N 个 file 共享 STS) |
| `TestConfirmUpload_Idempotent` | 集成(同 token 两次,只建一次 file) |
| `TestConfirmUpload_AlreadyConfirmed` | 集成 |
| `TestConfirmUpload_ExpiredSession` | 集成 |
| `TestCancelUpload_*` | 集成 |
| `TestRunUploadGC_OrphanCleanup` | 集成(HeadObject 返回存在 → 触发删除) |
| `TestStorageObject_VendorBucketMD5Unique` | 集成(跨 vendor 同 md5 允许共存) |

## 配置项汇总

```yaml
storage:
  sts:
    default_ttl: 15m         # 调用方未传 TTL 时用
    max_ttl: 1h              # 调用方传的 TTL 上限
  upload_session:
    ttl: 15m                 # session 过期(默认沿用 sts.default_ttl)
    dedup_lock:
      prefix: "upload:dedup"
      ttl: 10s
      tries: 3
      wait: 100ms
  cron:
    timezone: "Asia/Shanghai"
  upload_gc:
    cron_spec: "*/5 * * * *"  # 每 5 分钟
    batch_size: 100            # RunOnce 内部用
  batch:
    max_size: 100             # 单次批量请求上限
    concurrency: 10           # 并发处理数
```

## 兼容性 / 迁移

- **Proto**:
  - GetSTSCredential 加 `ttl` 字段(向后兼容,旧客户端不传走默认)
  - BatchGetSTSCredential 新增
  - CancelUpload 新增
  - ConfirmUpload 入参出参**不变**(session_id 嵌在 token 里)
- **DB**:
  - 新增 `upload_sessions` 表
  - `storage_objects`:`provider` 改 `vendor` 列(类型变 smallint),drop `idx_storage_objects_md5_size`,建 `idx_storage_objects_vendor_bucket_md5`
  - 当前无数据,AutoMigrate 直接操作
- **历史 token**:旧 HMAC token 没有 session_id → ConfirmUpload 验证时**直接拒绝**。项目还在快速迭代,内部调用,一刀切不做兼容层。

## 关键决策

| 决策 | 选择 | 理由 |
|---|---|---|
| Token 模型 | 保留 HMAC token,加 session_id | 客户端 API 零变更;token 防篡改,session 是状态源 |
| Quota 模型 | 不预占,只 check | reserve 复杂度不值得;OSS 孤儿 GC 兜底 |
| ObjectKey | `<prefix>/<md5>` 确定性函数 | 去重语义不变;session pin 一个具体 key,GC 精准清理 |
| MD5 去重约束 | `(vendor, bucket, md5)` 唯一 | 跨 vendor/bucket 物理隔离;同维度内共享 StorageObject |
| STS 凭证缓存 | 用户维度 + Redis,调用方传 TTL | 减少 STS API 调用;安全(用户隔离);TTL 业务方决定 |
| Session 去重 | DB 查重 + Redis 锁(redisx.NewLock) | 复用 PENDING session,锁防并发 |
| 批量接口 | BatchGetSTSCredential,共享 STS + N token | 减少 N 次 RPC RTT,共享 STS 凭证 |
| GC 调度 | `RunOnce(ctx)` + cronx,`option.WithCron` 注入 cron 实例 | 跟 db/redis 一致的注入模式;in-process 调用方可共享已有 cron |
| CancelUpload RPC | 新增 | 客户端显式取消比纯 GC 友好 |
| 历史兼容 | 旧 token 直接拒绝 | 项目早期,没必要做兼容层 |
| UploadSession 软删 | 保留 deleted_at | 审计需要历史 |

## 实施顺序

1. StorageObject 改造:`Provider` → `Vendor`,唯一约束改 `(vendor, bucket, md5)`,迁移索引
2. models.UploadSession + 迁移
3. uploadToken 加 session_id
4. STS 凭证缓存(Redis + 用户维度锁)
5. GetSTSCredential 改造(含 session 去重 + 锁)
6. BatchGetSTSCredential RPC
7. ConfirmUpload 改造(含幂等)
8. CancelUpload RPC
9. GC 实现(`RunOnce` + `option.WithCron` 注入,不传则服务自己从 config 创建)
10. 审计动作 + `UploadSessionSnapshot`
11. 测试覆盖

可以与 [[2026-06-16 STS Token 实现:Aliyun OSS 与 AWS S3|sts-token-implementation-design]] **并行实施**,本设计的 GetSTSCredential 改造部分依赖 STS 实现完成。

## 关联

- [[2026-06-16 STS Token 实现:Aliyun OSS 与 AWS S3|sts-token-implementation-design]] —— STS provider 实现,本设计的 prerequisite
- 实施计划:待通过 writing-plans skill 生成,路径 `docs/superpowers/plans/2026-06-16-upload-session-plan.md`
