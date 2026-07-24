# Aliyun STS Implementation Design

- **Date**: 2026-06-23
- **Status**: Pending review
- **Scope**: `internal/provider/storage/aliyun.go`, `internal/provider/storage/aliyunsts/`, `internal/provider/storage/sts_types.go`, `internal/service/upload/upload.go`, `api/proto/storage/v1/storage.proto`, `pkg/config/config.go`
- **Out of scope**: AWS S3 STS（保持现状不支持）, 文件大小 cloud 端强制（阿里云 OSS PutObject 路径不支持，依赖现有 reap.go 后置清理）

## Background

当前 `AliyunProvider.GetSTSToken` (`internal/provider/storage/aliyun.go:162-164`) 是 placeholder：

```go
func (AliyunProvider) GetSTSToken(_ context.Context, _ *STSPolicy) (*STSCredential, error) {
    return nil, fmt.Errorf("STS requires RoleARN configuration")
}
```

**`AliyunProvider` 结构体根本没有 RoleARN 字段**——error message 暗示的"配置"在代码里不存在。整个 STS 调用链从未真正打通，`GetSTSCredential` RPC 对阿里云用户完全不可用。

`STSPolicy` (`internal/provider/storage/sts_types.go:8-13`) 也只是空壳：

```go
type STSPolicy struct {
    Bucket    string
    KeyPrefix string
    MaxSize   int64
    TTL       time.Duration
}
```

没有 `AllowedExtensions`、`AllowedActions`，且所有 provider 都用 `_ *STSPolicy` 忽略参数。

## Goal

让 `GetSTSCredential` / `BatchGetSTSCredential` 对阿里云 OSS 真正可用：
- 用户在 RAM 控制台配好 RAM 角色 + RoleARN
- 用户在 `config.yaml` 填 RoleARN
- 业务调 `GetSTSCredential` 时通过 `allowed_extensions` 字段传后缀白名单
- 阿里云 AssumeRole 签发的临时凭证带 `Action=["oss:PutObject"]` + `Resource=[.../*.jpg, .../*.png]` 收紧策略
- 客户端 SDK 用临时凭证 PUT 时，OSS 服务端校验 key 后缀和操作类型，越权直接拒绝

## Design

### 1. 配置层：RoleARN 放 ProviderConfig

`pkg/config/config.go:178-187` 加字段：

```go
type ProviderConfig struct {
    Name      string
    Vendor    string
    Endpoint  string
    Region    string  // 已存在；阿里云用于推算 STS endpoint（sts.<region>.aliyuncs.com）
    AccessKey string
    SecretKey string
    // RoleARN 是阿里云 RAM 角色 ARN，仅 VENDOR_ALIYUN_OSS 用。
    // 格式：acs:ram::<account-id>:role/<role-name>
    // 在 RAM 控制台创建角色后获得；配置 STS 路径必填，不填则该 provider 的 STS
    // 不可用（GetSTSToken 返回明确错误）。S3/MinIO provider 忽略此字段。
    RoleARN   string
    Buckets   []*BucketConfig
}
```

`config.example.yaml` 加示例：

```yaml
storage:
  providers:
    - name: aliyun-prod
      vendor: VENDOR_ALIYUN_OSS
      region: cn-hangzhou                                # 已存在；推算 STS endpoint 用
      endpoint: oss-cn-hangzhou.aliyuncs.com
      access_key: ${ALIYUN_AK}
      secret_key: ${ALIYUN_SK}
      role_arn: "acs:ram::1234567890:role/storage-uploader"   # 新增
      buckets:
        - ...
```

**Region 处理**：复用现有 `ProviderConfig.Region` 字段，不新增。`NewAliyunProvider` 接收 region，**在构造时一次性算出 STS endpoint**（`sts.cn-hangzhou.aliyuncs.com`），固化为 provider 实例字段，整个生命周期复用。运行时 `GetSTSToken` 不需要再传 region。

**为什么不放 STSConfig**：RoleARN 是阿里云特有概念（AWS 用不同格式），跟 vendor 强绑定。一个项目可能配多个阿里云账号（生产/测试），各用不同 RoleARN——放 ProviderConfig 才能 per-provider 区分。

### 2. proto 层：加 `allowed_extensions`

`api/proto/storage/v1/storage.proto` 两个请求消息加字段：

```proto
message GetSTSCredentialRequest {
  Owner owner = 255;
  string bucket = 1;
  int64 max_size = 4;
  string md5 = 5;
  string content_type = 6;
  string filename = 7;
  google.protobuf.Duration ttl = 8;
  // 业务侧传入的后缀白名单，翻译到 STS policy Resource 通配符列表，由 OSS
  // 服务端强制。每个元素必须以 '.' 开头（如 ".jpg"），否则 BAD_REQUEST。
  // 空 = 不限制后缀。大小写不敏感（service 归一化为小写）。
  repeated string allowed_extensions = 9;
}

message BatchGetSTSCredentialRequest {
  Owner owner = 255;
  string bucket = 1;
  google.protobuf.Duration ttl = 2;
  repeated UploadFileMeta files = 3;
  // Batch 级别（整批共用一个凭证）。原因：per-file 会导致每个文件单独签一次
  // 凭证，违背 batch 节省 round-trip 的初衷。
  repeated string allowed_extensions = 4;
}
```

`GenerateUploadURLRequest` **不加**——presigned URL 路径不走 STS，后缀限制对 presigned PUT 无意义（OSS 在 PUT 时只校验签名，不校验后缀白名单）。

### 3. STSPolicy 扩展

`internal/provider/storage/sts_types.go`：

```go
type STSPolicy struct {
    Bucket            string
    KeyPrefix         string
    AllowedExtensions []string        // 新增：业务侧传，必带 '.' 前缀，已归一化为小写
    AllowedActions    []string        // 新增：默认 ["oss:PutObject"]，service 写死不让业务改
    MaxSize           int64           // ⚠️ service 层校验用，不进 STS policy JSON（阿里云 PutObject 不支持）
    TTL               time.Duration
}
```

### 4. aliyun 子包（集中所有阿里云代码）

新建 `internal/provider/storage/aliyun/`，把**现有** `storage/aliyun.go` 的 OSS 代码搬过来，同时新增 STS 代码。所有阿里云特有逻辑集中在一处，避免散落在 storage 包和独立子包之间。

```
internal/provider/storage/aliyun/
├── provider.go       (AliyunProvider struct + 现有 OSS 操作：PutObject/GetObject/...)
├── sts.go            (GetSTSToken + buildAliyunPolicy + stsEndpointFor)
├── sts_client.go     (raw STS SDK wrapper: Client + New + AssumeRole)
├── provider_test.go  (OSS 部分单测)
└── sts_test.go       (policy 翻译 + GetSTSToken 单测)
```

**sts_client.go**（封装阿里云官方 SDK，跟外部接口解耦）：

```go
package aliyun

import (
	"fmt"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	sts "github.com/alibabacloud-go/sts-20150401/v2/client"
	"github.com/alibabacloud-go/tea/tea"
)

// stsClient wraps the Aliyun STS SDK. Policy translation stays in sts.go so
// it can be unit-tested without a real STS endpoint.
type stsClient struct {
	cli *sts.Client
}

type stsClientOpts struct {
	AccessKeyId     string
	AccessKeySecret string
	RegionId        string
	Endpoint        string
	ConnectTimeout  *int
	ReadTimeout     *int
}

type assumeRoleReq struct {
	RoleArn         string
	RoleSessionName string
	DurationSeconds *int32
	Policy          map[string]any
}

type assumeRoleResp struct {
	RequestId       string
	AccessKeyId     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      string  // ISO8601 字符串，由调用方解析
}

func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	c, err := sts.NewClient(&openapi.Config{
		AccessKeyId:     tea.String(opts.AccessKeyId),
		AccessKeySecret: tea.String(opts.AccessKeySecret),
		RegionId:        tea.String(opts.RegionId),
		Endpoint:        tea.String(opts.Endpoint),
		ReadTimeout:     opts.ReadTimeout,
		ConnectTimeout:  opts.ConnectTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("create sts client: %w", err)
	}
	return &stsClient{cli: c}, nil
}

func (c *stsClient) assumeRole(req *assumeRoleReq) (*assumeRoleResp, error) {
	if req == nil {
		return nil, fmt.Errorf("nil assume role req")
	}
	r := &sts.AssumeRoleRequest{
		RoleArn:         tea.String(req.RoleArn),
		DurationSeconds: req.DurationSeconds,
		RoleSessionName: tea.String(req.RoleSessionName),
	}
	if req.Policy != nil {
		policyBytes, err := marshalPolicyJSON(req.Policy)
		if err != nil {
			return nil, fmt.Errorf("marshal policy: %w", err)
		}
		r.Policy = tea.String(string(policyBytes))
	}
	resp, err := c.cli.AssumeRole(r)
	if err != nil {
		return nil, fmt.Errorf("assume role: %w", err)
	}
	if resp == nil || resp.Body == nil || resp.Body.Credentials == nil {
		return nil, fmt.Errorf("assume role returned empty credentials")
	}
	return &assumeRoleResp{
		RequestId:       tea.StringValue(resp.Body.RequestId),
		AccessKeyId:     tea.StringValue(resp.Body.Credentials.AccessKeyId),
		AccessKeySecret: tea.StringValue(resp.Body.Credentials.AccessKeySecret),
		SecurityToken:   tea.StringValue(resp.Body.Credentials.SecurityToken),
		Expiration:      tea.StringValue(resp.Body.Credentials.Expiration),
	}, nil
}

// marshalPolicyJSON marshals the policy map with HTML escaping disabled —
// Aliyun policy JSON must not escape `<`, `>`, `&` or AssumeRole rejects it.
func marshalPolicyJSON(p map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}
```

**移植要点**：
- 现有 `storage/aliyun.go` 的所有 OSS 方法（PutObject/GetObject/DeleteObject/HeadObject/ListObjects/PresignPutObject/PresignGetObject/AliyunClient）整体搬到 `aliyun/provider.go`，包名从 `storage` 改成 `aliyun`
- 类型不变：`AliyunProvider` 实现 `storage.Provider` 接口（在 `storage` 包定义），aliyun 子包 import `storage` 拿接口和 STSPolicy/STSCredential
- `storage/registry.go` import `storage-service/internal/provider/storage/aliyun`，调用 `aliyun.NewAliyunProvider(...)`
- `img.NewAliyunProcessor(p.AliyunClient())` 不受影响——`AliyunClient()` 返回 `*oss.Client`，img 包拿到的还是同一个类型

### 5. AliyunProvider 真实现

`internal/provider/storage/aliyun/provider.go`（从 `storage/aliyun.go` 移植）改动：

```go
package aliyun

import (
    "time"
    // ... OSS SDK imports ...

    "storage-service/internal/provider/storage"
)

type AliyunProvider struct {
    client    *oss.Client
    endpoint  string
    accessKey string
    secretKey string
    region    string
    roleARN   string
    stsCli    *stsClient  // 包内类型；nil 表示未配 RoleARN，STS 不可用
}

func NewAliyunProvider(endpoint, accessKey, secretKey, roleARN, region string) (*AliyunProvider, error) {
    client, err := oss.New(endpoint, accessKey, secretKey)
    if err != nil {
        return nil, fmt.Errorf("create oss client: %w", err)
    }

    p := &AliyunProvider{
        client:    client,
        endpoint:  endpoint,
        accessKey: accessKey,
        secretKey: secretKey,
        region:    region,
        roleARN:   roleARN,
    }

    // RoleARN 配了才初始化 STS client。
    if roleARN != "" {
        stsCli, err := newSTSClient(&stsClientOpts{
            AccessKeyId:     accessKey,
            AccessKeySecret: secretKey,
            RegionId:        region,
            Endpoint:        stsEndpointFor(region),
        })
        if err != nil {
            return nil, fmt.Errorf("create sts client: %w", err)
        }
        p.stsCli = stsCli
    }
    return p, nil
}
```

`stsEndpointFor(region)` 是包内私有 helper，把 region 字符串（如 `"cn-hangzhou"`）映射成 STS 服务接入点（`sts.cn-hangzhou.aliyuncs.com`）。规则：`sts.<region>.aliyuncs.com`，对地区不在表里的兜底返回默认（`sts.cn-hangzhou.aliyuncs.com`）并 slog.Warn。**这个函数只在 `NewAliyunProvider` 构造时调用一次**——参数 region 来自 `cfg.Region`（`ProviderConfig` 已有字段，从 `config.yaml` 读），算出的 endpoint 固化到 AliyunProvider 的 `stsCli` 实例里，运行时不再重算。

`normalizeExtensions(exts []string) []string` 是包内私有 helper：每个元素 `strings.ToLower(strings.TrimSpace(...))`，过滤掉空字符串。

func (p *AliyunProvider) GetSTSToken(_ context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
    if p == nil || p.stsCli == nil || p.roleARN == "" {
        return nil, fmt.Errorf("aliyun STS not configured for this provider; set provider.role_arn in config")
    }

    policyJSON, err := buildAliyunPolicy(policy)
    if err != nil {
        return nil, fmt.Errorf("build sts policy: %w", err)
    }

    duration := int32(policy.TTL.Seconds())
    roleSession := fmt.Sprintf("owner-%d", policy.OwnerID)  // 审计可追溯

    resp, err := p.stsCli.assumeRole(&assumeRoleReq{
        RoleArn:         p.roleARN,
        RoleSessionName: roleSession,
        DurationSeconds: &duration,
        Policy:          policyJSON,
    })
    if err != nil {
        return nil, fmt.Errorf("aliyun sts assume role: %w", err)
    }

    expiresAt, err := time.Parse(time.RFC3339, resp.Expiration)
    if err != nil {
        return nil, fmt.Errorf("parse sts expiration %q: %w", resp.Expiration, err)
    }

    return &storage.STSCredential{
        AccessKey:       resp.AccessKeyId,
        SecretKey:       resp.AccessKeySecret,
        SecurityToken:   resp.SecurityToken,
        Endpoint:        p.endpoint,
        Bucket:          policy.Bucket,
        ObjectKeyPrefix: policy.KeyPrefix,
        ExpiresAt:       expiresAt,
    }, nil
}
```

**`buildAliyunPolicy` 翻译规则**（同包私有函数）：

```go
func buildAliyunPolicy(p *STSPolicy) (map[string]any, error) {
    actions := p.AllowedActions
    if len(actions) == 0 {
        actions = []string{"oss:PutObject"}
    }

    prefix := strings.TrimSuffix(p.KeyPrefix, "/")
    base := fmt.Sprintf("acs:oss:*:*:%s/%s/*", p.Bucket, prefix)

    var resources []string
    if len(p.AllowedExtensions) > 0 {
        for _, ext := range p.AllowedExtensions {
            if !strings.HasPrefix(ext, ".") {
                return nil, fmt.Errorf("extension %q must start with '.'", ext)
            }
            resources = append(resources, base+ext)
        }
    } else {
        resources = []string{base}
    }

    return map[string]any{
        "Version": "1",
        "Statement": []map[string]any{{
            "Effect":   "Allow",
            "Action":   actions,
            "Resource": resources,
        }},
    }, nil
}
```

**注意 STSPolicy 需要加 OwnerID 字段**用于 RoleSessionName（当前没有）：

```go
type STSPolicy struct {
    OwnerID          int64           // 新增：用于 RoleSessionName 审计追踪
    OwnerType        int32           // 新增：完整标识 owner
    Bucket           string
    KeyPrefix        string
    AllowedExtensions []string
    AllowedActions    []string
    MaxSize           int64
    TTL               time.Duration
}
```

### 6. service 层校验（fail-fast）

`internal/service/upload/upload.go` 的 `issueUploadCredential`：

```go
stsPolicy := &storage.STSPolicy{
    OwnerID:           ownerID,
    OwnerType:         ownerType,
    Bucket:            bucket,
    KeyPrefix:         prepared.bucketCfg.KeyPrefix,
    AllowedExtensions: normalizeExtensions(req.GetAllowedExtensions()),  // 归一化小写
    AllowedActions:    []string{"oss:PutObject"},                        // 写死
    MaxSize:           file.size,
    TTL:               prepared.resolvedTTL,
}
```

`prepareUpload` 里加早返回校验：

```go
if len(req.GetAllowedExtensions()) > 0 && req.GetFilename() != "" {
    ext := strings.ToLower(filepath.Ext(req.GetFilename()))
    allowed := normalizeExtensions(req.GetAllowedExtensions())
    found := false
    for _, a := range allowed {
        if a == ext {
            found = true
            break
        }
    }
    if !found {
        return nil, xcodes.ErrBadRequest.New(
            fmt.Sprintf("filename %q extension %q not in allowed list %v",
                req.GetFilename(), ext, allowed))
    }
}
```

省一次 STS 调用配额（cloud 端本来也会拒绝，但早 fail 不消耗阿里云 STS 限流配额）。

### 7. registry 改造

`internal/provider/storage/registry.go:210-219` 的 `newProvider` 调用更新：

```go
case storagev1.Vendor_VENDOR_ALIYUN_OSS:
    p, err := NewAliyunProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN, cfg.Region)
    // ...
```

签名变了（多了 RoleARN + Region），所有 NewAliyunProvider 调用方都要更新。

### 8. STS 失败时的 fallback 行为

如果 RoleARN 没配：
- `GetSTSTredential` 返回明确错误："STS not configured for this provider; set provider.role_arn in config"
- **不静默降级到 presigned URL**——客户端应该明确知道走哪条路

业务侧应该根据 bucket 所属 provider 选 RPC：
- 阿里云 OSS（配了 RoleARN）→ `GetSTSCredential`
- S3 / MinIO / 阿里云 OSS（没配 RoleARN）→ `GenerateUploadURL`

## Testing

### 单元测试

**`internal/provider/storage/aliyunsts/`**：
- `TestClient_AssumeRole_Success`：用 `httptest.Server` mock STS API，断言请求体正确
- `TestClient_AssumeRole_APIError`：mock 403/500，断言错误 wrap 正确
- `TestClient_AssumeRole_NetworkTimeout`：mock slow response + ConnectTimeout

**`internal/provider/storage/aliyun_test.go`**（新增）：
- `TestBuildAliyunPolicy_NoExtensions`：空 AllowedExtensions → 单个 `*` Resource
- `TestBuildAliyunPolicy_WithExtensions`：[".jpg", ".png"] → 两个 Resource 通配符
- `TestBuildAliyunPolicy_BadExtensionFormat`："jpg"（无点）→ error
- `TestBuildAliyunPolicy_CustomActions`：覆盖 AllowedActions 默认值
- `TestAliyunProvider_GetSTSToken_NoRoleARN`：nil stsCli → 明确错误
- `TestAliyunProvider_GetSTSToken_Success`：mock stsCli.AssumeRole，验证 RoleSessionName 包含 OwnerID

**`internal/service/upload/upload_test.go`**：
- `TestGetSTSCredential_ExtensionRejectedEarly`：filename 不匹配 allowed_extensions → BAD_REQUEST（不调 STS）
- `TestGetSTSCredential_ExtensionCaseInsensitive`：".JPG" vs [".jpg"] → 通过

### 集成测试

**不跑真阿里云**——需要真凭证，跟 `s3_test.go` 用 testcontainer MinIO 一个思路但阿里云没 testcontainer。集成测试改成 mock-based：
- `TestSTS_PolicyTranslationEndToEnd`：service.issueUploadCredential → aliyunsts.AssumeRole（mock），断言 mock 收到的 Policy JSON 跟预期结构一致

### 不在本 spec 范围内的测试

- aliyun-oss-go-sdk 行为（已由 SDK 保证）
- 真 AssumeRole API（需要云资源，CI 跑不了）

## Acceptance Criteria

1. ✅ 配了 `role_arn` 的阿里云 provider 调 `GetSTSCredential` 能拿到真临时凭证
2. ✅ 没配 `role_arn` 的阿里云 provider 调 `GetSTSCredential` 返回明确错误，不 panic
3. ✅ `allowed_extensions=[".jpg", ".png"]` 翻译成阿里云 policy 的两个 Resource 元素
4. ✅ `filename="photo.exe"` + `allowed_extensions=[".jpg"]` → BAD_REQUEST（service 层早 fail）
5. ✅ `filename="photo.JPG"` + `allowed_extensions=[".jpg"]` → 通过（大小写归一化）
6. ✅ 临时凭证的 Action 默认收紧到 `["oss:PutObject"]`
7. ✅ RoleSessionName 包含 OwnerID（审计可追溯）
8. ✅ Expiration 正确解析成 time.Time（保证之前 safety margin 逻辑生效）
9. ✅ S3/MinIO provider 不受影响，`GetSTSToken` 仍返回 "not supported"

## Migration / Compatibility

- proto 加字段是非破坏性（旧客户端不传 `allowed_extensions` = 不限制后缀）
- `ProviderConfig.RoleARN` 是可选字段，不填等同现状（STS 不可用）
- `NewAliyunProvider` 签名变化——但只在 `registry.go` 和测试里调用，影响面小
- STSPolicy 加字段（OwnerID/OwnerType/AllowedExtensions/AllowedActions）是非破坏性
- `STSCredential` 结构不变，已存在的缓存逻辑不受影响

## Out of Scope

- AWS S3 STS 实现（保持 `S3Provider.GetSTSToken` 返回 "not supported"）
- 文件大小 cloud 端强制（阿里云 OSS PutObject 路径不支持，依赖现有 reap.go）
- PostObject 路径（跟当前 presigned URL / STS 设计不兼容，需要时单独 spec）
- STS policy 的 Condition 字段（Referer、IP、TLS）——YAGNI
- 自动配置 RAM 角色 + bucket policy 的运维脚本——人工配置即可
- `MaxSize` 在 service 层之外的强制——明确文档化是软限制

## Risks

1. **新增 SDK 依赖体积**：`alibabacloud-go/sts-20150401` 及其传递依赖（darabonba-openapi、tea 等）。需评估 go.sum 影响和编译时间。
2. **STS 限流**：阿里云 AssumeRole 有 QPS 限制。当前 `internal/service/sts` 的 Redis 缓存已经覆盖这点（同 owner+vendor+bucket 共用凭证），集成时确保走缓存路径。
3. **RoleARN 配错 fail-fast 位置**：在 `NewAliyunProvider`（启动时）还是 `GetSTSToken`（运行时）报错？倾向启动时不报（RoleARN 是可选的），运行时报（用错了 provider 才知道）。
4. **阿里云 STS SDK 不支持 context**：跟 aliyun-oss-go-sdk 一样，超时只能靠 ConnectTimeout/ReadTimeout 配置项，不能用 ctx.Cancel。文档化这个限制。
