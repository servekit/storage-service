# storage-service

通用存储服务。提供对象存储文件的上传、下载、管理（元数据、配额、软删除、生命周期、审计）等能力。
可独立部署为 gRPC 服务，也可作为 Go 模块 in-process 嵌入宿主进程。

## 功能特性

- **多供应商**：阿里云 OSS、AWS S3、腾讯云 COS、华为云 OBS、火山引擎 TOS、S3 兼容（MinIO 等）
- **多账号**：同一供应商可配置多个账号（如多个阿里云账号），按 `name` 区分
- **上传/下载**：预签名直传 URL + STS 临时凭证（`AssumeRole`）
- **CDN 加速**：按 bucket 配置 CDN 签名（阿里云 Type A / CloudFront Signed URL）
- **文件管理**：列表、分页、更新、删除、批量删除、软删除与保留期
- **配额**：按 owner（用户/业务方）的存储配额管理
- **管理员 API**：文件/配额/统计/provider/bucket 管理、owner 清理
- **审计日志**：用户侧与管理侧操作审计
- **两种部署形态**：独立 gRPC 服务 / in-process Go 模块（[`pkg.NewModule`](pkg/module.go)）

## 技术栈

| 层 | 选型 |
|---|---|
| 语言 | Go 1.26 |
| RPC | gRPC + grpc-gateway（HTTP/JSON） |
| 协议 | Protobuf（[buf](api/proto/storage/v1) + protovalidate） |
| 数据库 | PostgreSQL（GORM，无外键，应用层保证关系完整性） |
| 缓存/锁 | Redis（限流、分布式锁、STS 凭证缓存） |
| 配置 | Viper（`go-common/configx`） |
| ID 生成 | 雪花算法（依赖 [gid-service](https://github.com/servekit/gid-service)） |
| 公共库 | [`go-common`](https://github.com/servekit/go-common)（`dbx`/`redisx`/`configx`/`grpcx`/`xerr`/`logging`/`ratelimit`/`signalx`） |

## 依赖

### 外部服务

| 依赖 | 用途 | 备注 |
|---|---|---|
| **PostgreSQL** | 元数据、配额、审计、上传会话 | 表名带前缀（默认 `stor_`） |
| **Redis** | 限流、分布式锁、STS 凭证缓存、上传去重 | |
| **gid-service** | 雪花 ID 生成 | 两种模式（见下） |
| **对象存储账号** | 实际文件存储 | 至少一个 provider |

**gid-service 模式**（`third_party.gid.mode`）：

- `module`：in-process 嵌入，无需单独部署，需配 `machine_id` / `start_time`
- `grpc`：远程调用独立部署的 gid-service，需配 `target`（如 `localhost:19091`）

### Go 关键依赖

见 [`go.mod`](go.mod)。核心：`grpc`、`grpc-gateway`、`gorm`、`redis/go-redis`、`spf13/viper`、各云厂商 SDK、`buf`/`protovalidate`。

## 快速开始（本地）

### 前置条件

- Go 1.26+
- PostgreSQL、Redis 可达（本地或 Docker）

### 1. 准备配置

```bash
cp config.example.yaml config.yaml
cp .env.example .env
# 编辑 .env：填入真实的数据库密码、云存储密钥等（搜索 change-me）
```

### 2. 初始化数据库（GORM AutoMigrate）

```bash
make migrate        # 等价于 go run ./cmd/server migrate
```

### 3. 运行服务

```bash
make run            # 等价于 go run ./cmd/server/
```

- gRPC：`:19093`
- HTTP gateway：`:18083`

## 配置

配置采用**结构 + 值分离**的设计：

- **`config.yaml`**：结构骨架与拓扑（几个 provider、各自的 endpoint/bucket）。所有明文值用 `${VAR}` 占位引用环境变量。**可提交到 Git**（不含密钥）。
- **`.env`**：所有 `${VAR}` 的实际值（密钥、连接信息、环境差异）。**不提交 Git**（已在 `.gitignore`）。

加载时 `configx` 先用 `os.ExpandEnv` 展开 `config.yaml` 里的 `${VAR}`，再做校验。两份示例文件见 [`config.example.yaml`](config.example.yaml) 与 [`.env.example`](.env.example)。

### 配置文件查找顺序

1. `-config` 启动参数
2. `STORAGE_SERVICE_CONFIG` 环境变量
3. `./config.yaml`
4. `/etc/storage-service/config.yaml`

### 配置段速查

| 段 | 说明 |
|---|---|
| `server` | gRPC / gateway 监听地址 |
| `database` | PostgreSQL 连接（host/port/user/password/dbname/连接池/table_prefix） |
| `redis` | Redis 连接 |
| `storage.providers` | 对象存储供应商与账号（见下） |
| `storage.sts` | STS 临时凭证缓存（TTL、安全边界、单飞锁） |
| `storage.cdn` | GenerateCDNURL 的 TTL 上下限 |
| `storage.rate_limit` | 上传限流规则（按 owner 类型） |
| `storage` 其他 | 上传 token、默认配额、孤儿/软删除保留期 |
| `third_party.gid` | gid-service 模式与连接 |
| `log` | 日志级别与格式 |

### 多供应商 / 多账号配置

`${VAR}` 变量名完全自由，**每个账号用独立的变量名命名空间**即可区分。同一供应商配多个账号示例：

```yaml
storage:
  providers:
    - name: aliyun-main
      vendor: VENDOR_ALIYUN_OSS
      endpoint: ${ALIYUN_MAIN_ENDPOINT}
      access_key: ${ALIYUN_MAIN_AK}
      secret_key: ${ALIYUN_MAIN_SK}

    - name: aliyun-backup            # 同一供应商，第二个账号
      vendor: VENDOR_ALIYUN_OSS
      endpoint: ${ALIYUN_BACKUP_ENDPOINT}
      access_key: ${ALIYUN_BACKUP_AK}
      secret_key: ${ALIYUN_BACKUP_SK}
```

```dotenv
# .env
ALIYUN_MAIN_AK=...
ALIYUN_MAIN_SK=...
ALIYUN_BACKUP_AK=...
ALIYUN_BACKUP_SK=...
```

> ⚠️ **`.env` 不支持行内 `#` 注释**：docker-compose 的 `env_file` 会把行尾 `# ...` 当成值的一部分。所有注释请独占一行（见 `.env.example`）。
>
> **不支持用扁平环境变量构造 providers 数组**：viper 的 `AutomaticEnv` 只能覆盖标量字段，无法填充/覆盖对象数组。数组结构必须写在 `config.yaml` 里，靠 `${VAR}` 注入值。

## 部署

### ⚠️ 安全前提（务必先读）

本服务**默认不带鉴权拦截器**。所有 RPC——包括 `AdminDeleteOwner`、`AdminDeleteFile`、`AdminSetQuota`、`SetOwnerQuota` 等高危管理接口——均可匿名调用，且请求里的 `Owner` 字段由调用方自行声明、服务端不校验。

**仅当**部署在可信边界之内时才可直接暴露：

- 位于已做鉴权的 API 网关 / service mesh / sidecar 之后
- 作为 Go 模块嵌入宿主进程，由宿主实施鉴权

在 `pkg.NewServer` 增加 auth 拦截器之前，**不要**将 `:19093`（gRPC）或 `:18083`（gateway）直接暴露到不可信网络。

### 二进制部署

```bash
make build                 # 产出 bin/storage-service（server + migrate 合一）
./bin/storage-service migrate   # 初始化/迁移数据库
./bin/storage-service          # 启动服务（等价于 ./bin/storage-service serve）
```

### Docker 部署

镜像是单二进制 `storage-service`，`migrate` 子命令跑迁移（靠 ENTRYPOINT 参数透传）。示例 `docker-compose.yml`：

```yaml
services:
  postgres:
    image: postgres:17
    environment:
      POSTGRES_USER: ${DB_USER}
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_DB: ${DB_NAME}
    volumes:
      - pgdata:/var/lib/postgresql/data

  redis:
    image: redis:7

  storage-service:
    build: .
    env_file: .env
    volumes:
      # 挂载到 configx 默认搜索路径，无需额外指定 -config
      - ./config.yaml:/etc/storage-service/config.yaml:ro
    ports:
      - "19093:19093"
      - "18083:18083"
    depends_on:
      - postgres
      - redis

volumes:
  pgdata:
```

```bash
docker compose up -d postgres redis          # 先起依赖
docker compose run --rm storage-service migrate   # 跑迁移
docker compose up -d storage-service         # 启动服务
```

### 端口

| 端口 | 协议 | 用途 |
|---|---|---|
| 19093 | gRPC | 主 RPC 入口 |
| 18083 | HTTP | grpc-gateway（REST/JSON） |

## API

gRPC service：`storagev1.StorageService`，proto 定义见 [`api/proto/storage/v1`](api/proto/storage/v1)。

| 分类 | RPC |
|---|---|
| 上传 | `GenerateUploadURL`、`ConfirmUpload`、`CancelUpload` |
| STS | `GetSTSCredential`、`BatchGetSTSCredential` |
| 下载/处理/CDN | `GenerateDownloadURL`、`GenerateProcessURL`、`GenerateCDNURL` |
| 文件管理 | `ListMyFiles`、`ListMyFilesPaged`、`GetMyFile`、`UpdateMyFile`、`DeleteMyFile`、`BatchDeleteMyFiles` |
| 配额 | `GetMyQuota`、`SetOwnerQuota`、`AddOwnerQuota` |
| 管理 | `AdminListFiles`、`AdminGetFile`、`AdminDeleteFile`、`AdminGetQuota`、`AdminSetQuota`、`AdminGetStats`、`AdminListProviders`、`AdminListBuckets`、`AdminSoftDeleteOwnerFiles`、`AdminDeleteOwner` |
| 审计 | `ListMyAuditLogs`、`AdminListAuditLogs` |

通过 gateway 访问示例：`http://localhost:18083/v1/files`、`http://localhost:18083/v1/admin/stats`（REST 端点对应 proto 的 `google.api.http` 注解）。

## 开发

```bash
make proto       # buf generate —— 生成 gRPC/gateway/protovalidate 代码到 gen/
make generate    # gorm gen —— 生成 internal/store/generated
make test        # 带竞态检测的测试
make all         # fmt + vet + lint + test
make tidy        # go mod tidy
```

测试：业务逻辑单元测试 + PostgreSQL 集成测试（`dbx.SetupTestDB` testcontainer）、Redis 测试（miniredis）、对象存储测试（minio testcontainer）。

## 项目结构

```
storage-service/
├── api/proto/storage/    # Protobuf 定义
├── cmd/
│   └── server/           # 启动入口：serve（默认）+ migrate 子命令（单二进制）
├── gen/                  # protoc/buf 生成代码
├── internal/
│   ├── service/          # gRPC service 实现
│   ├── provider/         # 对象存储 provider（各云厂商）
│   ├── store/            # 数据库（models / generated / dal）
│   ├── thirdcall/        # 第三方服务调用（gid-service）
│   ├── jobs/             # 后台任务（孤儿 GC 等）
│   └── utils/
├── pkg/                  # 可被外部 import
│   ├── config/           # 配置加载
│   ├── option/           # 功能选项（DI）
│   ├── xcodes/           # 业务错误码
│   ├── server.go         # gRPC server 封装
│   ├── client.go         # gRPC client 封装
│   └── module.go         # in-process 模块入口
├── config.example.yaml   # 配置模板（${VAR} 占位）
├── .env.example          # 环境变量模板
├── Dockerfile
├── Makefile
└── go.mod
```

## 作为 Go 模块使用（in-process）

无需独立部署，直接嵌入宿主进程：

```go
import (
    "github.com/servekit/storage-service/pkg"
    "github.com/servekit/storage-service/pkg/config"
)

cfg, err := config.Load()
if err != nil { return err }

hdl, err := pkg.NewModule(cfg)   // 实现 storagev1.StorageServiceServer
if err != nil { return err }
defer hdl.Stop()
```

`NewModule` 返回的 `Handler` 同时满足 gRPC server 接口与生命周期管理接口，可直接注册到宿主的 gRPC server。
