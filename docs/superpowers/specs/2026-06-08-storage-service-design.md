# Storage Service 设计文档

> 版本：v3 | 日期：2026-06-08
> v3 变更：去掉 Redis 依赖、upload_id 改为签名 token、去掉临时文件夹、直传最终路径、ConfirmUpload 用 INSERT ON CONFLICT 保证原子性、孤儿清理从云存储侧扫描

## 1. 项目定位

通用文件存储服务。支持多云后端（阿里云 OSS、腾讯 COS、华为 OBS、AWS S3、GCS、Azure Blob），提供客户端直传鉴权（预签名 URL + STS）、MD5 去重秒传、用户空间配额、图片处理 URL 生成、用户文件管理、管理接口。

可独立部署为 gRPC 服务，也可作为 Go 模块 in-process 使用。

## 2. 核心设计决策

### 2.1 存储与图片处理分层

- **存储操作**：Put / Get / Delete / Presign / STS — 通过 `Provider` 接口统一
- **图片处理**：通过 `ImageProcessor` 接口生成带处理参数的 URL，各厂商实现自己的 URL 格式
- 不支持原生图片处理的厂商返回 ErrUnsupportedOperation

### 2.2 两表设计（物理文件 + 用户映射）

- **storage_objects**：物理文件，MD5 全局去重，同一文件只存一份
- **user_files**：用户文件映射，同一物理文件可被多用户持有，每个用户有独立的文件名/路径/元数据

### 2.3 MD5 去重 + 秒传

- 客户端上传前计算 MD5
- 服务端检查 MD5：已存在则直接创建映射，无需上传（秒传）
- 并发上传同一 MD5：不等待不锁定，谁先确认谁创建 storage_objects，后来的自动复用

### 2.4 upload_id 为签名 token

- GenerateUploadURL 返回的 upload_id 是 HMAC-SHA256 签名的 token
- Token 自包含：user_id, md5, size, content_type, bucket, filename, file_path, metadata, expires_at
- 不可伪造、不可猜测、自带过期校验
- ConfirmUpload 时验证签名 + 过期时间 + user_id 归属
- **不依赖任何服务端存储**

### 2.5 直传最终路径

- 预签名 URL 指向最终路径 `{prefix}/{md5[:2]}/{md5}`
- 无临时文件夹、无 CopyObject
- storage_objects 记录只在 ConfirmUpload 成功后创建
- 并发冲突通过 `INSERT ON CONFLICT (md5) DO NOTHING` 原子解决

### 2.6 配额按用户独立计算

同一文件被多用户持有时，每个用户的配额都扣该文件的全额大小。

### 2.7 软删除

所有文件操作都是软删除（`deleted_at`），物理删除由后台清理任务在保留期后执行。

### 2.8 无 Redis 依赖

服务不依赖 Redis。上传会话通过签名 token 自包含，并发冲突通过数据库唯一约束解决。

## 3. 架构总览

```
┌──────────────────────────────────────────────────────┐
│                    gRPC / Gateway                     │
├──────────────────────────────────────────────────────┤
│                    Service Layer                      │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐ ┌───────┐ │
│  │  Upload   │  │  Quota   │  │  File    │ │ Image │ │
│  │  Manager  │  │  Manager │  │  Manager │ │ Proc  │ │
│  └─────┬────┘  └────┬─────┘  └────┬─────┘ └───┬───┘ │
├────────┼─────────────┼────────────┼────────────┼─────┤
│        │      Repository Layer    │            │     │
│  ┌─────┴────┐  ┌────┴─────┐ ┌────┴────┐ ┌────┴───┐ │
│  │ObjectRepo│  │QuotaRepo │ │UserFile │ │Provider│ │
│  │(Postgres)│  │(Postgres)│ │Repo     │ │Registry│ │
│  └──────────┘  └──────────┘ └─────────┘ └────┬───┘ │
├───────────────────────────────────────────────┼─────┤
│                Provider Layer                  │     │
│  ┌──────────┐ ┌──────────┐ ┌─────────┐       │     │
│  │ S3       │ │ Aliyun   │ │ Tencent │       │     │
│  │ Compat   │ │ OSS      │ │ COS     │       │     │
│  └──────────┘ └──────────┘ └─────────┘       │     │
│  ┌────────────────────────────────────────────┘     │
│  │        Image Processor Adapters                  │
│  │  oss-process │ imageView2 │ x-image-process      │
│  └──────────────────────────────────────────────────┘
└──────────────────────────────────────────────────────┘
```

## 4. 多云 Provider 设计

### Provider 接口

```go
// 存储操作 — 纯文件 CRUD
type Provider interface {
    PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...PutOption) error
    GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
    DeleteObject(ctx context.Context, bucket, key string) error
    HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error)

    PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration) (string, http.Header, error)
    PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)

    GetSTSToken(ctx context.Context, policy *STSPolicy) (*STSCredential, error)

    // 清理用：列出指定前缀下的 objects
    ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error)
}

// 图片处理 URL 生成 — 独立接口
type ImageProcessor interface {
    ProcessURL(ctx context.Context, bucket, key string, ops []ImageOp, ttl time.Duration) (string, error)
}
```

### Provider 分类

| Provider | 存储实现 | 图片处理 | 备注 |
|----------|---------|---------|------|
| AWS S3 | aws-sdk-go-v2 | 不支持原生 | 可搭配 CloudFront + Lambda@Edge |
| Aliyun OSS | aliyun-oss-go-sdk | `x-oss-process` | 推荐原生 SDK |
| Tencent COS | cos-go-sdk-v5 | `imageView2` / `imageMogr2` | 后续实现 |
| Huawei OBS | obs-sdk | `x-image-process` | 后续实现 |
| Google GCS | cloud.google.com/storage | 不支持原生 | 后续实现 |
| Azure Blob | azure-sdk-for-go | 不支持原生 | 后续实现 |
| S3 Compatible | aws-sdk-go-v2 | 不支持 | MinIO 等 |

### 图片处理 URL 格式

| 厂商 | URL 示例 |
|------|---------|
| 阿里云 | `image.jpg?x-oss-process=image/resize,w_200/format,webp` |
| 腾讯 | `image.jpg?imageView2/1/w/200/h/200/format/webp` |
| 华为 | `image.jpg?x-image-process=image/resize,m_pad,w_200` |
| S3/GCS/Azure | 返回 ErrUnsupportedOperation |

## 5. 上传流程

### 5.1 upload_id 签名 Token

GenerateUploadURL / GetSTSCredential 返回的 upload_id 是一个签名 token：

```go
type UploadToken struct {
    UserID      int64              `json:"uid"`
    MD5         string             `json:"md5"`
    Size        int64              `json:"size"`
    ContentType string             `json:"ct"`
    Bucket      string             `json:"bkt"`
    Filename    string             `json:"fn"`
    FilePath    string             `json:"fp"`
    Description string             `json:"desc"`
    Metadata    map[string]string  `json:"meta"`
    IsPublic    bool               `json:"pub"`
    ExpiresAt   int64              `json:"exp"`  // Unix timestamp
}

// 签名：HMAC-SHA256(json_bytes, server_secret_key)
// 编码：base64url(hmac_bytes) + "." + base64url(json_bytes)
// 验证：解析 + 校验签名 + 校验过期 + 校验 user_id
```

### 5.2 GenerateUploadURL 流程

```
Client                          Storage Service
  │                                    │
  │  GenerateUploadURL(filename,       │
  │    size, md5, content_type,        │
  │    bucket, file_path, ...)         │
  │───────────────────────────────────>│
  │                                    │
  │                      1. 查 storage_objects WHERE md5=? AND deleted_at IS NULL
  │                                    │
  │                      ┌─ 命中（active）→ 秒传
  │                      │   检查配额 → 创建 user_file → 更新配额 → ref_count++
  │                      │   返回 {instant: true, file_id, file_info}
  │                      │
  │                      └─ 未命中 → 正常上传
  │                          生成 object_key: {bucket.key_prefix}/{md5[:2]}/{md5}
  │                          生成 UploadToken（含所有参数 + 30min 过期）
  │                          签名并编码为 upload_id
  │                          Provider.PresignPutObject(bucket, object_key, 30min)
  │                          返回 {instant: false, upload_id, upload_url, headers}
  │                                    │
  │  upload_id, url, headers           │
  │<───────────────────────────────────│
  │                                    │
  │  PUT file + Content-MD5 header     │
  │  → 直传到云存储最终路径             │
```

### 5.3 ConfirmUpload 流程

```
Client                          Storage Service              Cloud Storage
  │                                    │                            │
  │  ConfirmUpload(upload_id)          │                            │
  │───────────────────────────────────>│                            │
  │                                    │                            │
  │                      1. 解析 upload_id token                    │
  │                      2. 验证签名 + 过期时间                      │
  │                      3. 验证 user_id（token 中的 uid = 调用者）  │
  │                                    │                            │
  │                      4. HEAD 最终路径 → 确认文件存在             │
  │                                    │──────────────────────────>│
  │                                    │<──────────────────────────│
  │                                    │                            │
  │                      5. 对比 MD5（token 中的 md5 vs HEAD ETag） │
  │                                    │                            │
  │                      6. 事务开始：                               │
  │                         INSERT storage_objects                  │
  │                           (status=active, md5, size, ...)      │
  │                           ON CONFLICT (md5) DO NOTHING         │
  │                                    │                            │
  │                         ┌─ 插入成功（我是第一个）                │
  │                         │   创建 user_files                    │
  │                         │   ref_count = 1                      │
  │                         │   更新配额 used_bytes += size         │
  │                         │                                      │
  │                         └─ 冲突（别人先确认了）                  │
  │                             SELECT storage_objects WHERE md5=? │
  │                             创建 user_files                    │
  │                             ref_count++                        │
  │                             更新配额 used_bytes += size         │
  │                                    │                            │
  │                      7. 事务提交                                 │
  │                                    │                            │
  │  file_id, file_info                │                            │
  │<───────────────────────────────────│                            │
```

### 5.4 STS 上传流程

与 GenerateUploadURL 类似，区别在于返回 STS 临时凭证而非预签名 URL：
- 秒传检查逻辑相同
- 非秒传时：生成 UploadToken + 调用 Provider.GetSTSToken + 返回凭证
- 确认流程相同

### 5.5 MD5 校验

- 预签名 URL：客户端 PUT 时携带 `Content-MD5` header，云存储校验
- STS：ConfirmUpload 时服务端 HEAD object，对比 ETag
- Token 中包含 md5，与 HEAD 结果对比

### 5.6 孤儿文件清理

由于上传前不创建 DB 记录，未确认的文件在云存储中无对应 DB 记录。

清理策略：
```
定期任务（如每小时）：
1. Provider.ListObjects(bucket, prefix) 列出云存储中的 objects
2. 批量查询 storage_objects，找出不在 DB 中的 object_key
3. 这些是孤儿文件（上传但未确认）
4. 检查文件的上传时间（LastModified）> 保留期（如 2 小时）
5. 删除孤儿文件
```

优化：用 `prefix/{md5[:2]}/` 按目录分批列出，避免一次列出全量。

## 6. 数据库设计

### 6.1 storage_objects（物理文件，MD5 去重）

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | bigint | PK | 雪花 ID |
| provider_type | varchar(32) | NOT NULL | 云厂商 |
| bucket | varchar(128) | NOT NULL | 云存储 bucket |
| object_key | varchar(512) | NOT NULL | 云存储路径 `{prefix}/{md5[:2]}/{md5}` |
| md5 | varchar(32) | UNIQUE WHERE deleted_at IS NULL, NOT NULL | 文件 MD5 |
| sha256 | varchar(64) | | 文件 SHA256 |
| size | bigint | NOT NULL | 文件大小（bytes） |
| content_type | varchar(128) | NOT NULL | MIME 类型 |
| extension | varchar(16) | | 扩展名 |
| etag | varchar(128) | | 云厂商 ETag |
| storage_class | varchar(32) | DEFAULT 'STANDARD' | 存储类型 |
| ref_count | int | NOT NULL DEFAULT 0 | 引用计数 |
| deleted_at | timestamptz | | 软删 |
| created_at | timestamptz | NOT NULL | 确认时间（即上传完成时间） |
| updated_at | timestamptz | NOT NULL | |

索引：
- `idx_storage_objects_md5` UNIQUE (md5) WHERE deleted_at IS NULL
- `idx_storage_objects_bucket_key` (bucket, object_key)

### 6.2 user_files（用户文件映射）

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | bigint | PK | 雪花 ID |
| user_id | bigint | NOT NULL | 所属用户 |
| object_id | bigint | NOT NULL | 关联 storage_objects.id |
| filename | varchar(256) | NOT NULL | 用户原始文件名 |
| file_path | varchar(512) | | 用户自定义路径 |
| description | text | | 描述 |
| metadata | jsonb | | 自定义元数据 |
| is_public | boolean | DEFAULT false | 是否公开 |
| deleted_at | timestamptz | | 软删 |
| created_at | timestamptz | NOT NULL | |
| updated_at | timestamptz | NOT NULL | |

索引：
- `idx_user_files_user_id` (user_id) WHERE deleted_at IS NULL
- `idx_user_files_object_id` (object_id)
- `idx_user_files_user_path` (user_id, file_path) WHERE deleted_at IS NULL

### 6.3 user_quotas（用户配额）

| 字段 | 类型 | 约束 | 说明 |
|------|------|------|------|
| id | bigint | PK | |
| user_id | bigint | UNIQUE, NOT NULL | |
| total_bytes | bigint | NOT NULL | 总配额 |
| used_bytes | bigint | NOT NULL DEFAULT 0 | 已使用 |
| created_at | timestamptz | NOT NULL | |
| updated_at | timestamptz | NOT NULL | |

约束：`CHECK (used_bytes >= 0 AND used_bytes <= total_bytes)`

定期对账：统计 user_files JOIN storage_objects 活跃文件总 size vs used_bytes。

## 7. gRPC API 设计

```protobuf
service StorageService {
    // ==================== 上传 ====================

    rpc GenerateUploadURL(GenerateUploadURLRequest) returns (GenerateUploadURLResponse);
    rpc GetSTSCredential(GetSTSCredentialRequest) returns (GetSTSCredentialResponse);
    rpc ConfirmUpload(ConfirmUploadRequest) returns (ConfirmUploadResponse);

    // ==================== 下载 ====================

    rpc GenerateDownloadURL(GenerateDownloadURLRequest) returns (GenerateDownloadURLResponse);

    // ==================== 用户文件管理 ====================

    rpc ListMyFiles(ListMyFilesRequest) returns (ListMyFilesResponse);
    rpc GetMyFile(GetMyFileRequest) returns (UserFileInfo);
    rpc UpdateMyFile(UpdateMyFileRequest) returns (UserFileInfo);
    rpc DeleteMyFile(DeleteMyFileRequest) returns (google.protobuf.Empty);
    rpc BatchDeleteMyFiles(BatchDeleteMyFilesRequest) returns (BatchDeleteMyFilesResponse);

    // ==================== 图片处理 ====================

    rpc GenerateProcessURL(GenerateProcessURLRequest) returns (GenerateProcessURLResponse);

    // ==================== 配额 ====================

    rpc GetMyQuota(google.protobuf.Empty) returns (QuotaInfo);

    // ==================== 管理（Admin） ====================

    rpc AdminListFiles(AdminListFilesRequest) returns (AdminListFilesResponse);
    rpc AdminGetFile(AdminGetFileRequest) returns (AdminFileInfo);
    rpc AdminDeleteFile(AdminDeleteFileRequest) returns (google.protobuf.Empty);
    rpc AdminGetQuota(AdminGetQuotaRequest) returns (QuotaInfo);
    rpc AdminSetQuota(AdminSetQuotaRequest) returns (QuotaInfo);
    rpc AdminGetStats(AdminGetStatsRequest) returns (AdminGetStatsResponse);
    rpc AdminCreateProvider(AdminCreateProviderRequest) returns (ProviderInfo);
    rpc AdminListProviders(google.protobuf.Empty) returns (AdminListProvidersResponse);
    rpc AdminCreateBucket(AdminCreateBucketRequest) returns (BucketInfo);
    rpc AdminListBuckets(google.protobuf.Empty) returns (AdminListBucketsResponse);
}
```

### 核心 Message

```protobuf
// ==================== 上传 ====================

message GenerateUploadURLRequest {
    string filename = 1;
    int64  size = 2;
    string md5 = 3;
    string content_type = 4;
    string bucket = 5;
    string file_path = 6;
    string description = 7;
    map<string, string> metadata = 8;
    bool   is_public = 9;
}

message GenerateUploadURLResponse {
    bool   instant = 1;                   // true=秒传
    int64  file_id = 2;
    UserFileInfo file_info = 3;

    // 非秒传时有效
    string upload_id = 10;                // 签名 token
    string upload_url = 11;               // 预签名 PUT URL
    string object_key = 12;
    map<string, string> headers = 13;     // Content-MD5 等
}

message GetSTSCredentialRequest {
    string bucket = 1;
    int64  max_size = 2;
    string filename = 3;
    string md5 = 4;
    string content_type = 5;
    string file_path = 6;
    string description = 7;
    map<string, string> metadata = 8;
    bool   is_public = 9;
}

message GetSTSCredentialResponse {
    bool   instant = 1;
    int64  file_id = 2;
    UserFileInfo file_info = 3;

    string upload_id = 10;
    string access_key = 11;
    string secret_key = 12;
    string security_token = 13;
    string endpoint = 14;
    string bucket = 15;
    string object_key = 16;
    int64  expires_at = 17;
}

message ConfirmUploadRequest {
    string upload_id = 1;                 // 签名 token
}

message ConfirmUploadResponse {
    int64  file_id = 1;
    UserFileInfo file_info = 2;
}

// ==================== 下载 ====================

message GenerateDownloadURLRequest {
    int64 file_id = 1;
    int32 ttl_seconds = 2;
}

message GenerateDownloadURLResponse {
    string download_url = 1;
    int64  expires_at = 2;
}

// ==================== 用户文件管理 ====================

message ListMyFilesRequest {
    string path_prefix = 1;
    string extension = 2;
    string content_type_prefix = 3;
    string order_by = 4;                  // created_at / filename / size
    bool   descending = 5;
    int32  page_size = 6;
    string page_token = 7;
}

message ListMyFilesResponse {
    repeated UserFileInfo files = 1;
    int32 total_count = 2;
    string next_page_token = 3;
}

message GetMyFileRequest {
    int64 file_id = 1;
}

message UpdateMyFileRequest {
    int64  file_id = 1;
    optional string filename = 2;
    optional string file_path = 3;
    optional string description = 4;
    map<string, string> metadata = 5;
    optional bool is_public = 6;
}

message DeleteMyFileRequest {
    int64 file_id = 1;
}

message BatchDeleteMyFilesRequest {
    repeated int64 file_ids = 1;
}

message BatchDeleteMyFilesResponse {
    int32 deleted_count = 1;
    repeated int64 failed_ids = 2;
}

// ==================== 图片处理 ====================

message ImageProcessOp {
    enum Type { RESIZE = 0; CROP = 1; QUALITY = 2; FORMAT = 3; WATERMARK = 4; ROTATE = 5; }
    Type type = 1;
    int32 width = 2;
    int32 height = 3;
    string format = 4;
    int32 quality = 5;
    string resize_mode = 6;
    string watermark_text = 7;
    int32 rotate_degrees = 8;
}

message GenerateProcessURLRequest {
    int64 file_id = 1;
    repeated ImageProcessOp ops = 2;
    int32 ttl_seconds = 3;
}

message GenerateProcessURLResponse {
    string url = 1;
    int64  expires_at = 2;
}

// ==================== 配额 ====================

message QuotaInfo {
    int64 total_bytes = 1;
    int64 used_bytes = 2;
    int64 available_bytes = 3;
    int32 file_count = 4;
}

// ==================== 文件详情 ====================

message UserFileInfo {
    int64  id = 1;
    string filename = 2;
    string file_path = 3;
    string description = 4;
    map<string, string> metadata = 5;
    bool   is_public = 6;
    int64  size = 10;
    string content_type = 11;
    string extension = 12;
    string md5 = 13;
    string created_at = 20;
    string updated_at = 21;
}

// ==================== Admin ====================
// Admin Messages 结构类似，增加 user_id 查询维度，此处省略
```

## 8. 目录结构

```
storage-service/
├── api/proto/storage/v1/
│   └── storage.proto
├── cmd/server/
│   ├── main.go
│   └── main_test.go
├── gen/storage/v1/
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── xcodes/
│   │   ├── storage.go
│   │   ├── quota.go
│   │   └── provider.go
│   ├── store/
│   │   ├── models/
│   │   │   ├── object.go
│   │   │   ├── user_file.go
│   │   │   └── quota.go
│   │   ├── generated/
│   │   └── repository/
│   │       ├── object_repo.go
│   │       ├── user_file_repo.go
│   │       └── quota_repo.go
│   ├── provider/
│   │   ├── provider.go
│   │   ├── registry.go
│   │   ├── s3.go
│   │   ├── aliyun.go
│   │   └── image/
│   │       ├── processor.go
│   │       └── aliyun.go
│   ├── upload/
│   │   ├── token.go            # 签名 token 生成/验证
│   │   ├── token_test.go
│   │   └── cleanup.go          # 孤儿文件清理
│   ├── quota/
│   │   └── manager.go
│   ├── service/
│   │   └── storage_service.go
│   └── middleware/
│       └── auth.go
├── pkg/
│   ├── server.go
│   ├── module.go
│   └── client.go
├── migrations/
│   ├── 000001_init_schema.up.sql
│   └── 000001_init_schema.down.sql
├── docs/superpowers/
├── .claude/skills/
├── .gitea/workflows/ci.yml
├── CLAUDE.md
├── Makefile
├── config.example.yaml
├── go.mod
└── go.sum
```

## 9. 配置示例

```yaml
server:
  grpc_addr: ":9000"
  http_addr: ":8080"

database:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  dbname: storage_service
  sslmode: disable
  max_open_conns: 25
  max_idle_conns: 10
  conn_max_lifetime: 5m

storage:
  upload_token_ttl: 30m
  upload_token_secret: ${UPLOAD_TOKEN_SECRET}
  default_quota_bytes: 10737418240    # 10GB
  default_bucket: "default"
  cleanup_interval: 1h
  orphan_retention: 2h                # 未确认文件保留 2 小时
  soft_delete_retention: 168h         # 软删保留 7 天

  providers:
    - name: aliyun-primary
      type: aliyun_oss
      endpoint: oss-cn-hangzhou.aliyuncs.com
      access_key: ${ALIYUN_ACCESS_KEY}
      secret_key: ${ALIYUN_SECRET_KEY}

    - name: aws-us-east
      type: aws_s3
      region: us-east-1
      access_key: ${AWS_ACCESS_KEY}
      secret_key: ${AWS_SECRET_KEY}

  buckets:
    - name: default
      provider: aliyun-primary
      key_prefix: "uploads/"
      acl: private

log:
  level: info
  format: json
```

## 10. 安全考虑

- upload_id 为签名 token，不可伪造，自带过期和 user_id 校验
- Provider 的 access_key/secret_key 通过环境变量注入
- STS 凭证限制 Policy（指定 bucket/key 前缀，限制文件大小）
- 预签名 URL 合理 TTL（上传 30 分钟，下载 15 分钟）
- 用户只能操作自己的文件（auth context 校验 user_id）
- Admin 接口需要单独权限校验
- object_key 基于 MD5，不暴露用户信息

## 11. 后续扩展

- 分片上传（大文件 multipart upload）
- 文件版本控制
- CDN 集成
- 图片处理自建服务（libvips）
- 存储桶策略
- 生命周期管理
- 腾讯 COS / 华为 OBS / GCS / Azure 原生 SDK
