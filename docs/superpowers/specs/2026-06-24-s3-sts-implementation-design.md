# S3 Provider STS Implementation Design

**Date:** 2026-06-24
**Status:** Approved
**Owner:** moss

## 背景

storage-service 当前 `S3Provider.GetSTSToken` 直接返回 `"STS not supported by S3 provider"`，把整个 S3 系（AWS S3 / MinIO / Ceph RGW / LocalStack）的 STS 能力一棍子打死。这其实是个**伪 not supported**：

- AWS S3 通过独立的 AWS STS 服务（`sts.<region>.amazonaws.com`）AssumeRole 签发临时凭证
- MinIO / Ceph RGW / LocalStack 原生支持 STS API（与 S3 API 同 endpoint）
- `aws-sdk-go-v2/service/sts@v1.43.2` 已在依赖里（被 S3 SDK 间接拉进来）
- config 里已经有通用的 `RoleARN` 字段

之前 deferred 的原因是项目主线是 Aliyun OSS，S3 provider 最初主要给 MinIO 做 integration test。现在补齐。

## 目标

- `S3Provider.GetSTSToken` 真正实现，返回可用的 STS 临时凭证
- 支持 AWS S3 和 MinIO/S3-compatible 两种后端
- 与 Aliyun provider 的 STS 能力对齐（同样的 hardening：HTTPS-only、ACL 锁、Deny PutObjectAcl）
- 配置面改动最小（不引入新 config 字段）

## 非目标

- PostObject 上传方式（单独的 design doc）
- MultipartUpload / AppendObject（与 MD5 内容寻址冲突，不做）
- STS token 缓存层（已有 `internal/service/sts` 的 cache + singleflight，这次不动）

## 设计

### 1. STS Endpoint 策略

复用现有 `Endpoint` 字段做模式判断，不新增 config：

| `Endpoint` 值 | 模式 | STS endpoint |
|---|---|---|
| `""` (空) | AWS 原生 | `https://sts.<Region>.amazonaws.com` |
| `"http://localhost:9000"` 等 | MinIO / S3-compatible | 复用 `Endpoint` 本身 |

判断依据：`NewS3Provider` 已经用 `endpoint != ""` 切 `UsePathStyle`，同一个 heuristic 用于 STS。

### 2. RoleARN 字段

复用 config 里现有的 `RoleARN`，更新注释（去掉 "Aliyun-only" 措辞）。

- AWS：`arn:aws:iam::<account-id>:role/<name>`
- MinIO：任意字符串（MinIO 服务端不强校验 ARN 格式，只在配置里启用 STS 即可）

`NewS3Provider` 签名加 `roleARN string` 参数，和 `NewAliyunProvider(endpoint, accessKey, secretKey, roleARN, region)` 对齐：

```go
func NewS3Provider(endpoint, region, accessKey, secretKey, roleARN string) (*S3Provider, error)
```

`roleARN == ""` 时 STS 不可用（GetSTSToken 返回 `"s3 STS not configured; set provider.role_arn"`），与 Aliyun 行为一致。

### 3. Policy 翻译（`buildS3Policy`）

完全对齐 `buildAliyunPolicy` 的 hardening，仅替换为 AWS IAM condition key 和 ARN 格式：

| STSPolicy 字段 | Aliyun（参考） | AWS S3 |
|---|---|---|
| EnforceHTTPS | `Bool.acs:SecureTransport=true` | `Bool.aws:SecureTransport=true` |
| LockObjectACL | `StringEquals.oss:x-oss-object-acl=private` | `StringEquals.s3:x-amz-acl=private` |
| DenyPutObjectACL | Action `oss:PutObjectAcl` | Action `s3:PutObjectAcl` |
| Resource | `acs:oss:<region>:<acct>:bucket/key/*` | `arn:aws:s3:::bucket/key/*`（**无 region/account 段**） |
| AllowedExtensions | `bucket/prefix/*.jpg` 每个扩展一行 | 同 |
| AllowedActions 默认 | `["oss:PutObject"]` | `["s3:PutObject"]` |

**关键差异**：S3 ARN 没有 region/account 段（S3 是全局服务），所以 `buildS3Policy(p)` 签名比 Aliyun 简单——不需要 `region`/`accountUID` 参数，也不需要 `parseAccountUID` / `orWildcard` 这些 helper。

MaxSize 同样不映射（AWS STS 也不支持 size limit），TTL 下限 900s 检查照搬（AWS STS 同样要求 `DurationSeconds >= 900`）。

### 4. 文件结构（按 golang-development skill §7）

新文件 `internal/provider/storage/s3/sts.go`：

```
package s3

import (...)

// === Types ===
type stsClient struct { ... }                // 包装 aws-sdk-go-v2/service/sts Client
type assumeRoleReq struct { ... }            // 项目侧的 request 类型
type assumeRoleResp struct { ... }
type assumeRoleCaller interface { ... }      // 测试 fake 用

// === Const ===
const minAWSSTSDuration int64 = 900

// === Constructor ===
func newSTSClient(opts *stsClientOpts) (*stsClient, error)

// === Exported methods ===
func (p *S3Provider) GetSTSToken(ctx, policy) (*STSCredential, error)

// === Unexported methods ===
func (c *stsClient) assumeRole(req) (*assumeRoleResp, error)

// --- internal helpers ---
func buildS3Policy(p *STSPolicy) (map[string]any, error)
func stsRegionalEndpoint(region string) string  // → "sts.<region>.amazonaws.com"
func marshalPolicyJSON(p) ([]byte, error)       // 关 HTML escaping，和 aliyun 一样
```

`internal/provider/storage/s3/provider.go` 改动：
- `S3Provider` struct 加 `stsCli assumeRoleCaller` 和 `roleARN string` 字段
- `NewS3Provider` 签名加 `roleARN` 参数；`roleARN != ""` 时初始化 stsClient
- 删除现有 stub `GetSTSToken`（移到 sts.go）

### 5. AWS STS SDK 使用

```go
import (
    awssts "github.com/aws/aws-sdk-go-v2/service/sts"
    awscfg "github.com/aws/aws-sdk-go-v2/config"
)
```

STS client 用静态凭证 + endpoint 派生：

```go
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
    awsCfg, err := awscfg.LoadDefaultConfig(context.Background(),
        awscfg.WithRegion(opts.Region),
        awscfg.WithCredentialsProvider(creds),
    )
    if err != nil { return nil, fmt.Errorf("load aws config: %w", err) }

    var stsOpts []func(*awssts.Options)
    if opts.Endpoint != "" {  // MinIO/custom
        stsOpts = append(stsOpts, func(o *awssts.Options) {
            o.BaseEndpoint = aws.String(opts.Endpoint)
        })
    }
    cli := awssts.New(awsCfg, stsOpts...)
    return &stsClient{cli: cli}, nil
}
```

`assumeRole` 直接调 `cli.AssumeRole(ctx, &awssts.AssumeRoleInput{...})`，把 Policy JSON 作为 `Policy` 字段塞进去。

### 6. Registry / Config 改动

- `pkg/config/config.go`：更新 `RoleARN` 字段注释，明确 "Aliyun RAM role ARN OR AWS IAM role ARN OR MinIO STS role identifier"
- `internal/provider/storage/registry.go:207`：`NewS3Provider` 调用加 `cfg.RoleARN` 参数
- `internal/service/upload/upload.go`：`issueUploadCredential` 已经按 vendor 分发（`AllowedActions: ["oss:PutObject"]`），需要给 S3 vendor 改成 `["s3:PutObject"]`

### 7. 测试

完全照搬 aliyun STS 那套（`internal/provider/storage/aliyun/sts_test.go`）：

- `fakeSTS` stand-in struct + `newS3ProviderWithFakeSTS` helper
- `TestBuildS3Policy_*`：覆盖 Extensions / Actions / KeyPrefix / EnforceHTTPS / LockObjectACL / DenyPutObjectACL / 空 Condition
- `TestS3Provider_GetSTSToken_*`：NoRoleARN / BelowMinTTL / Success / BadExpiration
- `TestAssumeRole_Success` / `TestAssumeRole_APIError`：用 `httptest.NewServer` mock STS API，verify Policy JSON 无 HTML escaping
- `TestNewSTSClient_NilOpts`

`s3/provider_test.go` 现有的测试调用 `NewS3Provider(endpoint, region, ak, sk)` 需要更新成 5 参数（加 `roleARN`，多数传 `""`）。

### 8. 错误码

不新增。复用现有的 `xcodes.ErrBadRequest` / `ErrInternal` 等。STS unavailable 走 `fmt.Errorf` 返回，由 service 层包装成合适的 xcode。

## 风险与权衡

| 风险 | 缓解 |
|---|---|
| AWS STS SDK 通过 `LoadDefaultConfig` 隐式找 credential chain，行为难测 | 用 `awscfg.WithCredentialsProvider` 显式注入静态凭证，不依赖环境变量 |
| MinIO STS 默认关闭，需要管理员显式启用 + 配置 Web IDC / LDAP | 文档里说明，不在代码里处理（运行时错误清晰即可） |
| `Endpoint` 字段含义超载（既表示 S3 endpoint 又表示 STS endpoint） | 文档明确；如果未来需要分离再加 `STSEndpoint` 字段 |
| AWS S3 全局命名空间，bucket 名跨账号冲突 | STS Policy 的 Resource 已 scope 到具体 bucket，client 拿到的 token 也只能访问声明的 bucket |

## 验证清单

- [ ] `go build ./...` 通过
- [ ] `golangci-lint run ./internal/provider/storage/s3/...` 通过（无新增 issue）
- [ ] 新增单元测试全绿（policy translation + GetSTSToken + httptest mock）
- [ ] 现有 `s3/provider_test.go` 调用点更新后全绿（除了预存在的 testcontainer endpoint 问题）
- [ ] `internal/service/upload/` 的 `issueUploadCredential` 按 vendor 切 Action 前缀，相关测试通过
- [ ] FakeProvider 不受影响（已经实现自己的 GetSTSToken）
