# CLAUDE.md — storage-service

## 项目定位

通用存储服务。提供文件上传、下载、管理（元数据、权限、生命周期）等能力。
可独立部署为 gRPC 服务，也可作为 Go 模块 in-process 使用。

## 技术栈约定

### Redis

- 使用 `go-common/redisx` 统一初始化：`redisx.New(cfg)` 创建客户端（含 Ping 验证）
- 客户端通过构造函数注入（`func NewXxx(client *redis.Client)`），不持有全局连接
- key 命名：`<module>:<purpose>:<identifier>`
- 测试用 `redisx.NewTestClient(t)` 做内存 Redis（miniredis 封装）

### 数据库 / GORM

- 使用 `go-common/dbx` 统一初始化：`dbx.New(cfg)` 创建连接（含连接池、slog 日志、GORM 配置）
- 使用 `gorm.io/cli` 做代码生成（参考 gorm-cli-development skill）
- Model 定义在 `internal/models/`，生成代码输出到 `internal/generated/`
- 数据库使用 PostgreSQL
- 迁移工具：GORM AutoMigrate，入口为 `cmd/server` 的 `migrate` 子命令（`go run ./cmd/server migrate`），支持建表、新增字段和索引
- **不使用外键约束（REFERENCES）**，关系完整性由应用层保证。只保留 UNIQUE 约束和索引
- GORM 模型不定义 `foreignKey` 关联字段（如 `User User`），只用 ID 字段（如 `UserID int64`）
- GORM 日志通过 dbx 内置 slog logger 输出，与 slog 统一，禁止 GORM 默认的 fmt 日志

### 数据库集成测试

- 使用 `dbx.SetupTestDB(t)` 启动 PostgreSQL testcontainer（已封装在 go-common）
- 每个测试用例前清理数据（truncate 或事务回滚），保证测试隔离
- Error path（连接失败、超时等）可用 `go-sqlmock` 做单元测试补充

### gRPC / Proto

- Proto 定义在 `api/proto/storage/`
- 使用 `protoc` + `grpc-gateway` 生成代码到 `gen/` 目录
- gRPC server 监听 `:9000`，grpc-gateway 监听 `:8080`

### 错误处理

- 返回的错误统一使用 `go-common/xerr`，不用裸 `fmt.Errorf` 或 `errors.New`
- 预定义业务错误码：`xerr.New(reason, category, httpCode, message)`
- **错误码集中在 `internal/xcodes/` 中按领域分文件定义**，避免散落在各模块造成重复
- 通用错误码直接用 `xcodes` 包：`xcodes.ErrNotFound`、`xcodes.ErrInternal` 等
- 创建错误：`xcodes.ErrXxx.New()` 或 `xcodes.ErrXxx.New("override message")`
- 包装底层错误：`xcodes.ErrXxx.Wrap(err)` 或 `xcodes.ErrXxx.Wrapf(err, "context: %d", id)`
- 禁止 panic，所有错误通过返回值传递
- 使用 `errors.Is()` / `errors.As()` 比较，xerr 已实现 `Unwrap()` 和 `Is()`

### 日志

- 使用标准库 `log/slog`，禁止 `fmt.Println`、`log.Println` 等非结构化输出
- 库代码（`internal/` 中的业务逻辑）不直接打日志，通过返回 error 交给调用方
- 只有 `cmd/server/` 入口层和 middleware 可以打日志
- slog 使用结构化参数：`slog.Error("msg", "key", value)`，不用 `slog.Sprintf`

### 依赖

- `go-common` — 错误码（`xerr`）、Redis（`redisx`）、数据库（`dbx`）

### 通用

- ID 生成：雪花算法
- 配置：YAML，用 `github.com/spf13/viper`
- 遵循 golang-development skill 的编码规范

### 文件内函数排列

- 导出的类型、构造函数、方法放在文件上部
- 未导出的辅助函数（`lowercase`）放在文件底部，用 `// --- internal helpers ---` 分隔
- 目的：打开文件即可看到公开 API，快速了解模块能力

### 错误处理

- 禁止擅自添加 `//nolint` 注释，必须显式处理每个 error
- 即使是辅助操作（审计日志、缓存失效等），也要显式处理 error，不允许用 `_ =` 忽略
- 唯一例外：资源清理（`Close()` 等）可以用 `_ =`

### 枚举优先

- 在 Proto 中定义了枚举的字段，service 层必须使用枚举类型传递，禁止硬编码字符串
- 字符串转换只在调用外部 API 边界时进行
- 新增字段优先考虑使用枚举（enum）而非 string + validation

## 代码质量

```bash
gofmt -w <file.go>
goimports -w <file.go>
golangci-lint run ./...
go test -race -coverprofile=coverage.out ./...
```

## 目录结构

```
storage-service/
├── api/proto/storage/       # Protobuf 定义
├── api/swagger/             # buf 生成的 OpenAPI/Swagger 2.0 文档（由 buf.gen.yaml 的 openapiv2 插件产出，供前端/客户端消费）
├── cmd/server/              # 启动入口：serve（默认）+ migrate 子命令（单二进制）
├── gen/                     # protoc 生成代码
├── internal/
│   ├── thirdcall/           # 第三方服务调用实现
│   │   └── gid_service/     # gid-service（module / gRPC）
│   ├── thirdcall/           # 第三方服务实现
│   │   └── gid_service/     # gid-service（module / gRPC）
│   ├── store/               # 数据库相关
│   │   ├── models/          # GORM Model 定义
│   │   ├── generated/       # gorm.io/cli 生成代码
│   │   └── repository/      # 数据库操作（使用 generated 代码）
│   ├── service/             # gRPC service 实现
│   └── middleware/          # gRPC 拦截器
├── pkg/                     # 可被外部 import
│   ├── config/              # 配置加载
│   ├── option/              # 功能选项（DI）
│   ├── thirdcall/           # 第三方服务接口（公开）
│   ├── xcodes/              # 业务错误码
│   ├── server.go            # gRPC server 封装
│   ├── client.go            # gRPC client 封装
│   └── module.go            # in-process 模块入口
├── CLAUDE.md
├── Makefile
├── config.yaml
├── go.mod
└── go.sum
```
