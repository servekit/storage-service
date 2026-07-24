# STS Token 实现:Aliyun OSS 与 AWS S3

日期:2026-06-16
分支:feat/audit-logging(开始),实施时建议切到 `feat/sts-token`

## 背景与动机

`internal/provider/aliyun.go` 和 `internal/provider/s3.go` 的 `GetSTSToken` 都是占位实现:

```go
// aliyun.go
func (AliyunProvider) GetSTSToken(_ context.Context, _ *STSPolicy) (*STSCredential, error) {
    return nil, fmt.Errorf("STS requires RoleARN configuration")
}

// s3.go
func (S3Provider) GetSTSToken(_ context.Context, _ *STSPolicy) (*STSCredential, error) {
    return nil, fmt.Errorf("STS not supported by S3 provider")
}
```

但 service 层(`internal/service/upload.go:350`)在 `GetSTSCredential` RPC 中真的会调用 `p.GetSTSToken(...)`,意味着客户端直传场景下这个 RPC 必然失败。客户端要么用 presigned URL(`generateUploadURL` 路径),要么拿到无效响应。

需要给两个 provider 都补上真正的 STS 调用,让 `GetSTSCredential` 可用。

## 目标

1. `AliyunProvider.GetSTSToken` 调用 Aliyun STS 服务,返回临时 AK/SK + SecurityToken
2. `S3Provider.GetSTSToken` 调用 AWS STS(AssumeRole),返回临时凭证
3. 凭证作用域严格限定到 STSPolicy 指定的 bucket + keyPrefix + maxSize
4. 未配置 STS 时返回清晰的业务错误码,而不是裸 `fmt.Errorf`
5. 配置项独立成 `STSConfig` 结构,便于扩展

## 当前状态

| 文件 | 现状 | 改动 |
|---|---|---|
| `internal/provider/aliyun.go` | 占位 + 报错 | 注入 STS client,实现 AssumeRole |
| `internal/provider/s3.go` | 占位 + 报错 | 注入 STS client,实现 AssumeRole |
| `pkg/config/config.go` | `ProviderConfig` 无 RoleARN | 新增 `STSConfig` 指针字段 |
| `pkg/xcodes/provider.go` | 无 STS 相关错误码 | 新增 `ErrSTSNotConfigured` |
| `internal/provider/registry.go` | `NewAliyunProvider` / `NewS3Provider` 4 参 | 多传一个 `*STSConfig`,内部按需构造 STS client |
| `internal/provider/s3_test.go` | 旧 `TestS3Provider_GetSTSToken_Unsupported` | 改为未配置 STS 场景,新增 policy 构造单测 |
| `config.example.yaml` | provider 无 sts 配置 | 加示例 `sts:` 块 |

## 改造方案

### 1. Config 扩展 (`pkg/config/config.go`)

```go
// STSConfig holds settings for issuing temporary credentials via cloud STS.
// Optional — nil means STS is unavailable for this provider; GetSTSToken
// will return ErrSTSNotConfigured.
type STSConfig struct {
    RoleARN     string // required when set; e.g. "acs:ram::<account>:role/<name>" or "arn:aws:iam::<account>:role/<name>"
    SessionName string // optional, default "storage-service"
    Endpoint    string // optional, Aliyun only; default "sts.aliyuncs.com"
}

type ProviderConfig struct {
    // ... existing fields unchanged ...
    STS *STSConfig
}
```

`Config.Validate()` 增加校验:若 `STS != nil`,则 `STS.RoleARN` 必填。

### 2. Provider 接口

接口不变。`Provider.GetSTSToken` 仍然在接口上,语义改为:
- `STSConfig == nil` → 返回 `ErrSTSNotConfigured`
- `STSConfig.RoleARN == ""` → 同上(防御性,Validate 已拦)
- STS API 调用失败 → 包装原始错误,不转业务错误码

### 3. Aliyun 实现 (`internal/provider/aliyun.go`)

**依赖**:`github.com/aliyun/aliyun-sts-go-sdk/sts`

**结构体**:

```go
type AliyunProvider struct {
    client    *oss.Client
    endpoint  string
    accessKey string
    secretKey string

    stsClient *sts.Client // nil if STSConfig == nil
    stsCfg    *config.STSConfig
}
```

**构造函数签名变化**:

```go
func NewAliyunProvider(endpoint, accessKey, secretKey string, stsCfg *config.STSConfig) (*AliyunProvider, error)
```

若 `stsCfg != nil`,用 `sts.NewClientWithAccessKey(endpoint, ak, sk)` 创建 client;endpoint 默认 `sts.aliyuncs.com`。

**GetSTSToken 流程**:

1. 检查 `stsCfg == nil` → 返回 `ErrSTSNotConfigured`
2. `ttl := clampTTL(stsPolicy.TTL, 900*time.Second, 3600*time.Second)`(若 0,默认 30min)
3. 构造 policy JSON:

```json
{
  "Version": "1",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["oss:PutObject"],
    "Resource": ["acs:oss:*:*:<bucket>/<keyPrefix>*"],
    "Condition": {
      "NumericLessThanEquals": {"oss:ContentLength": <maxSize>}
    }
  }]
}
```

`Condition` 仅当 `stsPolicy.MaxSize > 0` 时加入。

4. 调用 `client.AssumeRole(roleArn, sessionName, policy, ttlSeconds)`
5. 解析 `resp.Credentials`(`AccessKeyId` / `AccessKeySecret` / `SecurityToken` / `Expiration`)
6. 转换 Expiration 字符串(RFC3339)到 `time.Time`
7. 返回 `STSCredential{AccessKey, SecretKey, SecurityToken, Endpoint: p.endpoint, Bucket: stsPolicy.Bucket, ObjectKeyPrefix: stsPolicy.KeyPrefix, ExpiresAt: expiration}`

### 4. AWS S3 实现 (`internal/provider/s3.go`)

**依赖**:`github.com/aws/aws-sdk-go-v2/service/sts`

**结构体**:

```go
type S3Provider struct {
    client    *s3.Client
    presigner *s3.PresignClient

    stsClient *sts.Client // nil if STSConfig == nil
    stsCfg    *config.STSConfig
    endpoint  string
}
```

**构造函数签名变化**:

```go
func NewS3Provider(endpoint, region, accessKey, secretKey string, stsCfg *config.STSConfig) (*S3Provider, error)
```

若 `stsCfg != nil`,创建 `sts.New(sts.Options{Region: region, Credentials: creds, BaseEndpoint: endpoint})`。

**GetSTSToken 流程**:

1. 检查 `stsCfg == nil` → 返回 `ErrSTSNotConfigured`
2. `ttl := clampTTL(stsPolicy.TTL, 900*time.Second, 43200*time.Second)`(若 0,默认 30min)
3. 构造 policy JSON:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:PutObject"],
    "Resource": ["arn:aws:s3:::<bucket>/<keyPrefix>*"]
  }]
}
```

AWS IAM 不支持 ContentLength 条件,MaxSize 字段忽略;依赖应用层 `checkQuota` 校验。

4. 调用 `client.AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn, RoleSessionName, Policy, DurationSeconds})`
5. 解析 `out.Credentials`(`AccessKeyId` / `SecretAccessKey` / `SessionToken` / `Expiration`)
6. 返回 `STSCredential{AccessKey, SecretKey, SecurityToken, Endpoint: p.endpoint, Bucket, ObjectKeyPrefix, ExpiresAt}`

### 5. 错误码 (`pkg/xcodes/provider.go`)

```go
ErrSTSNotConfigured = xerr.New("STS_NOT_CONFIGURED", xerr.CategoryBadRequest, 400, "STS is not configured for this provider")
```

400 而非 500:这是配置错误,业务可恢复(改配置即可),不是服务端运行时故障。

### 6. Registry 改造 (`internal/provider/registry.go`)

`newProvider` 把 `cfg.STS` 透传给 `NewAliyunProvider` / `NewS3Provider`。

### 7. Policy 构造的复用

Aliyun 和 AWS policy JSON 结构差异较大(Version、Resource 格式、Condition),不强行抽公共函数。各 provider 内部各自有 `buildSTSPolicy(stsPolicy *STSPolicy) (string, error)`,加单元测试。

### 8. TTL 工具

`clampTTL(ttl, min, max time.Duration) time.Duration` 放在 `internal/provider/sts.go`(新文件),处理 0/超出范围的情况。两个 provider 共用。

## 关键决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 实现范围 | 两个 provider 都做 | 用户明确要求 |
| SDK 选择 | 官方 SDK | aws-sdk-go-v2 已在用,风格一致;Aliyun STS SDK 复杂签名交给库 |
| Config 结构 | 独立 STS struct | 扩展性好,语义清晰 |
| MaxSize 限制 | 能加则加 | Aliyun 支持,加 ContentLength;AWS IAM 不支持,跳过,靠 checkQuota |
| 错误处理 | 新增 ErrSTSNotConfigured | 与项目错误码风格一致;调用方可 `errors.Is` 判断 |
| SessionName | 可配置,默认 "storage-service" | 不同部署可区分 |

## 测试策略

| 测试 | 类型 | 文件 |
|---|---|---|
| `buildSTSPolicy_Aliyun_*` | 单元 | `aliyun_test.go`(新建) |
| `buildSTSPolicy_S3_*` | 单元 | `s3_test.go` |
| `clampTTL_*` | 单元 | `sts_test.go`(新建) |
| `GetSTSToken_NotConfigured` | 单元(无需 STS server) | 各 provider test |
| `GetSTSToken_RealCall` | 集成(可选,build tag `integration`) | 各 provider test |

集成测试需要真实 STS 凭证,默认跳过(`t.Skip` 当环境变量未设置)。

## 兼容性

- `NewAliyunProvider` / `NewS3Provider` 签名变化:新增 `stsCfg *config.STSConfig` 参数,可能为 nil
- `Provider` 接口不变,实现方法签名不变
- service 层调用方不变

## 不在范围内

- 不改 `STSPolicy` / `STSCredential` 结构体字段(只填值)
- 不动 `GetSTSCredential` RPC 协议
- 不动 ObjectKey 命名规则(目前客户端用 `ObjectKeyPrefix` 自行决定具体 key,这个设计问题留待后续)
- 不引入 STS 凭证缓存(每次 RPC 都调一次 STS;若 QPS 高,后续单独加 cache)

## 关联

待补充:本设计对应实施计划将写在 `docs/superpowers/plans/2026-06-16-sts-token-implementation-plan.md`(下一步通过 writing-plans skill 生成)
