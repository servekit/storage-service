# Storage Service 实施计划

> 日期：2026-06-08 | 基于设计文档 v3
> 关键决策：无 Redis、upload_id 签名 token、直传最终路径、INSERT ON CONFLICT

---

## Phase 1: 基础搭建

> 目标：项目骨架可编译运行，数据库 schema 就绪。

### Step 1.1: go.mod 依赖引入

引入依赖：

```
github.com/servekit/go-common  → ../go-common (replace)
gorm.io/gorm / gorm.io/driver/postgres
github.com/golang-migrate/migrate/v4
github.com/spf13/viper
google.golang.org/grpc / protobuf / grpc-gateway
github.com/aws/aws-sdk-go-v2                      # S3 兼容
github.com/aliyun/aliyun-oss-go-sdk                # 阿里云 OSS
github.com/aliyun/alibaba-cloud-sdk-go/services/sts
github.com/stretchr/testify
github.com/testcontainers/testcontainers-go
```

**验收**：`go mod tidy && go build ./...` 通过。

### Step 1.2: 配置加载

**文件**：`internal/config/config.go`

```go
type Config struct {
    Server   ServerConfig
    Database dbx.Config
    Storage  StorageConfig
    Log      LoggingConfig
}

type StorageConfig struct {
    UploadTokenTTL      time.Duration
    UploadTokenSecret   string
    DefaultQuotaBytes   int64
    DefaultBucket       string
    CleanupInterval     time.Duration
    OrphanRetention     time.Duration
    SoftDeleteRetention time.Duration
    Providers           []ProviderConfig
    Buckets             []BucketConfig
}

type ProviderConfig struct {
    Name      string
    Type      string  // aliyun_oss / aws_s3 / s3_compatible
    Endpoint  string
    Region    string
    AccessKey string
    SecretKey string
}

type BucketConfig struct {
    Name      string
    Provider  string
    KeyPrefix string
    ACL       string
}
```

Load 支持 `-config` flag、`STORAGE_SERVICE_CONFIG` 环境变量、`./config.yaml` 默认路径。环境变量替换用 viper AutomaticEnv。

**测试**：`internal/config/config_test.go`

**验收**：从 YAML 加载完整配置，环境变量替换生效。

### Step 1.3: 数据库迁移

**文件**：`migrations/000001_init_schema.up.sql`

```sql
CREATE TABLE storage_objects (
    id              bigint PRIMARY KEY,
    provider_type   varchar(32) NOT NULL,
    bucket          varchar(128) NOT NULL,
    object_key      varchar(512) NOT NULL,
    md5             varchar(32) NOT NULL,
    sha256          varchar(64),
    size            bigint NOT NULL,
    content_type    varchar(128) NOT NULL,
    extension       varchar(16),
    etag            varchar(128),
    storage_class   varchar(32) NOT NULL DEFAULT 'STANDARD',
    ref_count       int NOT NULL DEFAULT 0,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    updated_at      timestamptz NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_storage_objects_md5
    ON storage_objects (md5) WHERE deleted_at IS NULL;
CREATE INDEX idx_storage_objects_bucket_key
    ON storage_objects (bucket, object_key);

CREATE TABLE user_files (
    id              bigint PRIMARY KEY,
    user_id         bigint NOT NULL,
    object_id       bigint NOT NULL REFERENCES storage_objects(id),
    filename        varchar(256) NOT NULL,
    file_path       varchar(512),
    description     text,
    metadata        jsonb,
    is_public       boolean NOT NULL DEFAULT false,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    updated_at      timestamptz NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_user_files_user_id
    ON user_files (user_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_user_files_object_id
    ON user_files (object_id);
CREATE INDEX idx_user_files_user_path
    ON user_files (user_id, file_path) WHERE deleted_at IS NULL;

CREATE TABLE user_quotas (
    id              bigint PRIMARY KEY,
    user_id         bigint NOT NULL UNIQUE,
    total_bytes     bigint NOT NULL,
    used_bytes      bigint NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT NOW(),
    updated_at      timestamptz NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_quota CHECK (used_bytes >= 0 AND used_bytes <= total_bytes)
);
```

**文件**：`migrations/000001_init_schema.down.sql` — DROP TABLE 三张表。

**验收**：`migrate up` 成功。

### Step 1.4: GORM Model

**文件**：`internal/store/models/`

- `object.go`：StorageObject struct
- `user_file.go`：UserFile struct
- `quota.go`：UserQuota struct

GORM 约定：
- `deleted_at` 用 `*time.Time`（GORM 软删）
- 不定义外键关联，只用 ID 字段
- storage_objects 去掉 GORM 默认的 deleted_at 软删行为（手动管理），因为 MD5 唯一索引需要 WHERE deleted_at IS NULL

### Step 1.5: 错误码定义

**文件**：`internal/xcodes/`

- `storage.go`：ErrFileNotFound, ErrFileAlreadyExists, ErrMD5Mismatch, ErrFileSizeExceeded, ErrInvalidFilename, ErrInvalidFilePath, ErrUploadTokenExpired, ErrUploadTokenInvalid
- `quota.go`：ErrQuotaExceeded, ErrQuotaNotFound
- `provider.go`：ErrProviderNotFound, ErrBucketNotFound, ErrUnsupportedOperation

### Step 1.6: 启动入口

**文件**：`cmd/server/main.go`

```go
func main() {
    cfg := config.Load()
    logging.Setup(cfg.Log)
    srv, err := storageservice.NewServer(cfg)
    if err != nil { log.Exit(err) }
    srv.Run()
}
```

此阶段 NewServer 为空壳。

**验收**：`go build -o bin/server ./cmd/server/` 成功。

---

## Phase 2: Provider 层

> 目标：Provider 接口 + S3 兼容 + 阿里云 OSS + 图片处理可用。

### Step 2.1: Provider 接口

**文件**：`internal/provider/provider.go`

```go
type Provider interface {
    PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...PutOption) error
    GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
    DeleteObject(ctx context.Context, bucket, key string) error
    HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error)
    PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration) (string, http.Header, error)
    PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration) (string, error)
    GetSTSToken(ctx context.Context, policy *STSPolicy) (*STSCredential, error)
    ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error)
}
```

配套 struct：ObjectInfo, STSPolicy, STSCredential, PutOption。

### Step 2.2: ImageProcessor 接口

**文件**：`internal/provider/image/processor.go`

```go
type ImageProcessor interface {
    ProcessURL(ctx context.Context, bucket, key string, ops []ImageOp, ttl time.Duration) (string, error)
}

type ImageOp struct {
    Type          ImageOpType
    Width, Height int
    Format        string
    Quality       int
    ResizeMode    string
    WatermarkText string
    RotateDegrees int
}

// NopProcessor — 不支持图片处理的 Provider
type NopProcessor struct{}
```

### Step 2.3: Provider 注册表

**文件**：`internal/provider/registry.go`

```go
type Registry struct {
    providers       map[string]Provider
    imageProcessors map[string]ImageProcessor
    buckets         map[string]BucketConfig
}

func NewRegistry(providers []ProviderConfig, buckets []BucketConfig) (*Registry, error)
func (r *Registry) ProviderForBucket(bucket string) (Provider, error)
func (r *Registry) ImageProcessorForBucket(bucket string) (ImageProcessor, error)
func (r *Registry) BucketConfig(name string) (BucketConfig, error)
```

初始化：按 type 创建 Provider + ImageProcessor，建立 bucket → provider 映射。

**测试**：`internal/provider/registry_test.go`

### Step 2.4: S3 兼容实现

**文件**：`internal/provider/s3.go`

基于 aws-sdk-go-v2：
- NewS3Provider(endpoint, region, accessKey, secretKey)
- PutObject → s3manager.Uploader
- GetObject → s3.GetObject
- DeleteObject / HeadObject
- PresignPutObject / PresignGetObject → presigner
- GetSTSToken → sts.AssumeRole（需要 Role ARN）或返回 ErrUnsupportedOperation
- ListObjects → s3.ListObjectsV2

覆盖 AWS S3、华为 OBS（S3 兼容模式）、MinIO。

**测试**：`internal/provider/s3_test.go` — testcontainers MinIO。

### Step 2.5: 阿里云 OSS 实现

**文件**：`internal/provider/aliyun.go`

基于 aliyun-oss-go-sdk：
- NewAliyunProvider(endpoint, accessKey, secretKey)
- PutObject / GetObject / DeleteObject / HeadObject → oss.Client
- PresignPutObject / PresignGetObject → client.SignURL
- GetSTSToken → alibaba-cloud-sdk-go sts.AssumeRole
- ListObjects → client.ListObjects

**测试**：`internal/provider/aliyun_test.go`

### Step 2.6: 阿里云图片处理

**文件**：`internal/provider/image/aliyun.go`

转换 []ImageOp → `x-oss-process` 参数：
- resize → `image/resize,w_{w},h_{h},m_{mode}`
- format → `image/format,{fmt}`
- quality → `image/quality,q_{n}`
- 多操作 `/` 连接

**测试**：`internal/provider/image/aliyun_test.go` — 验证 URL 格式。

---

## Phase 3: Repository 层 + 签名 Token + 业务逻辑

> 目标：DB 操作层完成，upload_id token 签发/验证就绪，配额管理就绪。

### Step 3.1: ObjectRepo

**文件**：`internal/store/repository/object_repo.go`

```go
type ObjectRepo struct { db *gorm.DB }

func (r *ObjectRepo) FindByMD5(ctx context.Context, md5 string) (*models.StorageObject, error)
func (r *ObjectRepo) GetByID(ctx context.Context, id int64) (*models.StorageObject, error)

// 原子插入：INSERT ON CONFLICT (md5) DO NOTHING
// 返回 (object, inserted, error)
func (r *ObjectRepo) CreateOrGet(ctx context.Context, obj *models.StorageObject) (*models.StorageObject, bool, error)

func (r *ObjectRepo) IncrRefCount(ctx context.Context, id int64) error
func (r *ObjectRepo) DecrRefCount(ctx context.Context, id int64) error
func (r *ObjectRepo) SoftDelete(ctx context.Context, id int64) error

// 清理：软删超过保留期且 ref_count=0
func (r *ObjectRepo) FindPurgeable(ctx context.Context, before time.Time) ([]models.StorageObject, error)
func (r *ObjectRepo) HardDelete(ctx context.Context, id int64) error
```

**重点**：`CreateOrGet` 使用 `INSERT ... ON CONFLICT (md5) WHERE deleted_at IS NULL DO NOTHING`，配合 `RETURNING` 判断是否插入成功。

**测试**：testcontainers PostgreSQL。

### Step 3.2: UserFileRepo

**文件**：`internal/store/repository/user_file_repo.go`

```go
type UserFileRepo struct { db *gorm.DB }

func (r *UserFileRepo) Create(ctx context.Context, f *models.UserFile) error
func (r *UserFileRepo) GetByIDAndUser(ctx context.Context, id, userID int64) (*models.UserFile, error)

type ListUserFilesFilter struct {
    PathPrefix        string
    Extension         string
    ContentTypePrefix string
    OrderBy           string
    Descending        bool
    PageSize          int
    PageToken         string
}
func (r *UserFileRepo) ListByUser(ctx context.Context, userID int64, filter ListUserFilesFilter) ([]models.UserFile, int, error)

func (r *UserFileRepo) Update(ctx context.Context, f *models.UserFile) error
func (r *UserFileRepo) SoftDelete(ctx context.Context, id int64) error
func (r *UserFileRepo) BatchSoftDelete(ctx context.Context, ids []int64, userID int64) (int, error)
func (r *UserFileRepo) CountByUser(ctx context.Context, userID int64) (int64, error)
```

**测试**：testcontainers PostgreSQL。

### Step 3.3: QuotaRepo

**文件**：`internal/store/repository/quota_repo.go`

```go
type QuotaRepo struct { db *gorm.DB }

func (r *QuotaRepo) GetByUser(ctx context.Context, userID int64) (*models.UserQuota, error)
func (r *QuotaRepo) CreateIfNotExist(ctx context.Context, userID int64, totalBytes int64) (*models.UserQuota, error)

// 增加已用：UPDATE ... SET used_bytes = used_bytes + :bytes WHERE user_id = :uid AND used_bytes + :bytes <= total_bytes
func (r *QuotaRepo) IncrementUsed(ctx context.Context, userID int64, bytes int64) error
func (r *QuotaRepo) DecrementUsed(ctx context.Context, userID int64, bytes int64) error
func (r *QuotaRepo) SetQuota(ctx context.Context, userID int64, totalBytes int64) error
```

### Step 3.4: upload_id 签名 Token

**文件**：`internal/upload/token.go`

```go
type UploadToken struct {
    UserID      int64             `json:"uid"`
    MD5         string            `json:"md5"`
    Size        int64             `json:"sz"`
    ContentType string            `json:"ct"`
    Bucket      string            `json:"bkt"`
    Filename    string            `json:"fn"`
    FilePath    string            `json:"fp"`
    Description string            `json:"desc"`
    Metadata    map[string]string `json:"meta"`
    IsPublic    bool              `json:"pub"`
    ExpiresAt   int64             `json:"exp"`
}

// 签发
func SignToken(token *UploadToken, secret string) (string, error)

// 验证：签名校验 + 过期校验 + user_id 校验
func VerifyToken(encoded string, secret string, expectedUserID int64) (*UploadToken, error)
```

签名方式：HMAC-SHA256(json_bytes, secret) → base64url(hmac) + "." + base64url(json)

**测试**：`internal/upload/token_test.go`
- 签发 → 验证 → 通过
- 篡改 → 验证失败
- 过期 → 验证失败
- user_id 不匹配 → 验证失败

### Step 3.5: 配额管理

**文件**：`internal/quota/manager.go`

```go
type Manager struct {
    quotaRepo    *repository.QuotaRepo
    defaultBytes int64
}

func (m *Manager) CheckQuota(ctx context.Context, userID int64, requiredBytes int64) error
func (m *Manager) Reserve(ctx context.Context, userID int64, bytes int64) error
func (m *Manager) Release(ctx context.Context, userID int64, bytes int64) error
func (m *Manager) GetQuota(ctx context.Context, userID int64) (*models.UserQuota, int64, error)
```

### Step 3.6: 孤儿文件清理

**文件**：`internal/upload/cleanup.go`

```go
type Cleaner struct {
    registry  *provider.Registry
    objectRepo *repository.ObjectRepo
    retention  time.Duration
}

// 清理云存储中的孤儿文件
// 1. 遍历每个 bucket 的 objects（按 md5[:2] 分批）
// 2. 批量查 storage_objects
// 3. 不在 DB 中且 LastModified > retention 的文件 → 删除
func (c *Cleaner) CleanOrphanObjects(ctx context.Context) (int, error)

// 清理软删超过保留期且 ref_count=0 的记录
func (c *Cleaner) PurgeDeletedObjects(ctx context.Context) (int, error)
```

作为后台 goroutine 定期运行。

---

## Phase 4: gRPC API

> 目标：Proto 定义完成，Service 实现完成。

### Step 4.1: Proto 定义

**文件**：`api/proto/storage/v1/storage.proto`

按设计文档 Section 7 完整定义 StorageService + 所有 Message。

**验收**：`buf lint` 通过。

### Step 4.2: Buf 配置 + 代码生成

**文件**：`buf.yaml`, `buf.gen.yaml`

参考 user-service，适配 storage-service。

**验收**：`buf generate` 成功。

### Step 4.3: Service 实现 — 上传

**文件**：`internal/service/storage_service.go`

#### GenerateUploadURL

```
1. 校验参数
2. ObjectRepo.FindByMD5(md5)
3. 命中 → 秒传：CheckQuota → 创建 UserFile → IncrRefCount → Reserve → 返回 instant
4. 未命中：
   a. CheckQuota(userID, size)
   b. 生成 object_key: {bucket.key_prefix}/{md5[:2]}/{md5}
   c. 构建 UploadToken（所有参数 + 30min 过期）
   d. SignToken → upload_id
   e. Provider.PresignPutObject(bucket, object_key, 30min)
   f. 返回 upload_id + url + headers
```

#### ConfirmUpload

```
1. VerifyToken(upload_id) → 解析出所有参数 + 验证 user_id
2. Provider.HeadObject(bucket, object_key) → 确认文件存在
3. 对比 MD5（token 中的 md5 vs HEAD ETag）
4. 事务：
   a. ObjectRepo.CreateOrGet(storage_objects) → INSERT ON CONFLICT
      - inserted=true: 我是第一个 → 创建 UserFile, ref_count=1, Reserve
      - inserted=false: 别人先确认了 → 查已有记录, 创建 UserFile, IncrRefCount, Reserve
   b. 提交
5. 返回 file_id + file_info
```

#### GetSTSCredential

同 GenerateUploadURL 逻辑，秒传检查 + 非秒传返回 STS 凭证。

**测试**：秒传、正常上传、MD5 不匹配、配额不足、token 过期、并发确认。

### Step 4.4: Service 实现 — 下载 + 文件管理

- GenerateDownloadURL：查 UserFile → 获取 Object → PresignGetObject
- ListMyFiles：UserFileRepo.ListByUser + 批量查 Object
- GetMyFile：查 UserFile + Object → 组装 UserFileInfo
- UpdateMyFile：查 UserFile → 更新字段 → 保存
- DeleteMyFile：事务 SoftDelete UserFile + DecrRefCount + Release
- BatchDeleteMyFiles：批量 DeleteMyFile

### Step 4.5: Service 实现 — 图片处理 + 配额 + Admin

- GenerateProcessURL：查 UserFile → 获取 Object → ImageProcessor.ProcessURL
- GetMyQuota：QuotaManager.GetQuota + UserFileRepo.CountByUser
- Admin 系列 RPC

---

## Phase 5: 对外暴露

### Step 5.1: pkg/server.go

```go
func NewServer(cfg *config.Config) (*Server, error)
// 初始化 DB → Registry → Repo → Manager → Service → gRPC 注册
// 启动 cleanup goroutine
func (s *Server) Run() error
func (s *Server) Stop()
```

### Step 5.2: pkg/module.go

```go
type ModuleOption func(*moduleOptions)
func WithDB(db *gorm.DB) ModuleOption
func NewModule(cfg *config.Config, opts ...ModuleOption) (*Module, error)
```

### Step 5.3: pkg/client.go

```go
func NewClient(addr string, opts ...grpc.DialOption) (*Client, error)
func (c *Client) Close() error
// 代理所有 pb.StorageServiceClient 方法
```

---

## Phase 6: 测试 & 收尾

### Step 6.1: 集成测试

环境：testcontainers PostgreSQL + MinIO

测试用例：
1. 完整上传：GenerateUploadURL → PUT MinIO → ConfirmUpload → 验证 DB
2. 秒传：同一文件两次，第二次 instant=true
3. 并发上传同 MD5：两个请求，ConfirmUpload 后只有一个 storage_objects
4. 配额耗尽：上传到限额后拒绝
5. 删除流程：DeleteMyFile → 配额释放 → ref_count 减少
6. 文件管理：ListMyFiles / UpdateMyFile / BatchDeleteMyFiles
7. 图片处理 URL：验证 URL 参数格式
8. Token 安全：过期 token / 篡改 token / 错误 user_id 均被拒绝

### Step 6.2: CI 配置

`.gitea/workflows/ci.yml`：lint + test（需要 PostgreSQL + MinIO 服务）。

### Step 6.3: 收尾

`go mod tidy`，更新 CLAUDE.md 目录结构，确认 config.example.yaml 与实际一致。

---

## 依赖图

```
Phase 1 (基础) ─────────────┬──────────────────┐
                            │                  │
Phase 2 (Provider) ─────────┤   Phase 3 (Repo  │
                            │   + Token + Quota)│
                            └────────┬─────────┘
                                     │
                            Phase 4 (gRPC API)
                                     │
                            Phase 5 (Server/Module/Client)
                                     │
                            Phase 6 (测试 & 收尾)
```

Phase 2 和 Phase 3 可并行。
