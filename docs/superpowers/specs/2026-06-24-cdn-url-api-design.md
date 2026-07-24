# CDN URL API Design

- **Date**: 2026-06-24
- **Status**: Pending review (revised — switched to signed URL mode)
- **Scope**: `api/proto/storage/v1/storage.proto`, `internal/provider/storage/{types,aliyun,s3,fake,registry}/`, `internal/provider/storage/aliyun/cdnauth/`, `internal/service/file/`, `pkg/config/`, `pkg/xcodes/`, `config.example.yaml`, `go.mod` (新增 AWS CloudFront sign dep)
- **Out of scope**: Aliyun Type B/C/D（同构 Type A，未来按需补）, CloudFront Signed Cookies, Lambda@Edge

## Background

当前 storage-service 提供两种读路径：
- `GenerateDownloadURL` — presigned URL（OSS/S3 短时效签名 URL，直连 origin）
- `GenerateProcessURL` — 图片处理 URL（Aliyun OSS x-oss-process + SignURL）

两者都直连 OSS / S3，没经过 CDN。接入 CDN 后，CDN 节点缓存热点资源，客户端就近访问，OSS 只在缓存 miss 时被回源。

## Goal

提供新 RPC `GenerateCDNURL`，让客户端拿到 **CDN 鉴权 URL**（带签名+过期时间），覆盖纯下载和图片处理两个场景。

## Design

### 假设与限制

- **OSS bucket 是 private**，CDN 控制台开启 URL 鉴权（Aliyun Type A）或 CloudFront Signed URL
- **CDN URL 带签名 + 过期时间**：盗链窗口受 TTL 限制，过期失效
- **vendor 范围**：Aliyun OSS + CDN（Type A）；S3 + CloudFront（Signed URL via RSA）
- **图片处理**：
  - Aliyun：x-oss-process 透传（CDN 缓存按 URL 含参数分桶）
  - S3/CloudFront：暂不支持（未来可加 Lambda@Edge）
- **CDN 域名粒度**：per-provider
- **is_public 限制**：移除（CDN URL 自带访问控制，任何文件都能签）

### 架构 + 数据流

```
client.rpc GenerateCDNURL(file_id, ops, ttl)
       ↓
service/file.GetCDNURL
  1. dal.GetFileByID(file_id) → file + object_id
  2. ownership 检查（不匹配 → ErrFileNotFound，不泄露存在性）
  3. dal.GetObjectByID(object_id) → bucket + object_key
  4. registry.CDNURLGeneratorForBucket(bucket) → generator
       - generator == nil → ErrCDNNotConfigured
  5. resolveTTL(ttl) → clamp 到 [minTTL, maxTTL]，默认 1h
  6. conv: proto ops → types.Op
  7. generator.CDNURL(ctx, bucket, object_key, ops, ttl)
       - aliyun: 拼 https://<domain>/<key>?x-oss-process=...&auth_key=<signed>
       - s3 + ops 非空: ErrCDNImageProcessingUnsupported
       - s3 + ops 空: CloudFront Signed URL
  8. 返回 url 给 client
```

### 包结构改动

```
internal/provider/storage/
├── types/
│   ├── types.go           (现有)
│   ├── sts.go             (现有)
│   ├── errors.go          (现有)
│   └── cdn.go             ← NEW: CDNURLGenerator 接口 + ErrCDNNotConfigured + ErrCDNImageProcessingUnsupported
├── aliyun/
│   ├── provider.go        ← 加 cdnConfig 字段 + 构造函数参数
│   ├── cdn.go             ← NEW: (*AliyunProvider).CDNURL() 用 cdnauth 签名
│   ├── cdnauth/
│   │   └── type_a.go      ← NEW: Aliyun Type A 鉴权算法（自写 MD5）
│   ├── imgproc.go         (现有)
│   └── *_test.go
├── s3/
│   ├── provider.go        ← 加 cdnConfig 字段 + 构造函数参数
│   ├── cdn.go             ← NEW: (*S3Provider).CDNURL() 用 cloudfront/sign SDK
│   └── *_test.go
├── fake/
│   └── provider.go        ← 实现 CDNURLGenerator（占位）
├── imgproc/               (现有 — 不变)
├── provider.go            (现有别名)
├── sts_types.go           (现有别名)
├── errors.go              (现有别名)
└── registry.go            ← 加 CDNURLGeneratorForBucket
```

### Proto

```proto
service StorageService {
  ...
  // GenerateCDNURL returns a CDN-fronted signed URL (with expiry) for an
  // already-uploaded file. Caller must own the file. If ops is non-empty,
  // the URL carries image-processing parameters (Aliyun x-oss-process);
  // S3/CloudFront providers reject ops with CDN_IMAGE_PROCESSING_UNSUPPORTED.
  rpc GenerateCDNURL(GenerateCDNURLRequest) returns (GenerateCDNURLResponse) {
    option (api.get) = "/v1/files/{file_id}/cdn-url";
  }
}

message GenerateCDNURLRequest {
  // file_id is the already-uploaded file to generate a CDN URL for.
  int64 file_id = 1 [(buf.validate.field).int64 = {gt: 0}];
  // ops is optional. Empty = plain download URL; non-empty = image processing
  // URL (only supported by Aliyun OSS+CDN).
  // NOTE: no min_items validation — empty ops = plain download.
  repeated ImageProcessOp ops = 2;
  // ttl is optional. If unset, defaults to storage.cdn.default_ttl (1h).
  // Clamped to [min_ttl, max_ttl] = [5m, 24h].
  google.protobuf.Duration ttl = 3;
  Owner owner = 255;
  string request_id = 256;
}

message GenerateCDNURLResponse {
  string url = 1;        // https://<cdn-domain>/<object_key>?<auth>=...&[x-oss-process=...]
  int64 expires_at = 2;  // Unix timestamp when URL expires
}
```

复用现有 `ImageProcessOp` message。

### Provider Capability 接口

`internal/provider/storage/types/cdn.go`:

```go
package types

import (
    "context"
    "fmt"
    "time"
)

// ErrCDNNotConfigured is returned when the provider's bucket has no CDN
// configured (CDN is nil in ProviderConfig).
var ErrCDNNotConfigured = fmt.Errorf("cdn: not configured for this provider")

// ErrCDNImageProcessingUnsupported is returned when the provider does not
// support image processing at the CDN/origin (S3+CloudFront).
var ErrCDNImageProcessingUnsupported = fmt.Errorf("cdn: image processing not supported by this provider")

// CDNURLGenerator builds CDN-fronted signed URLs for objects.
type CDNURLGenerator interface {
    // CDNURL returns a CDN signed URL for the given object. The URL carries
    // a signature and expires at (now + ttl). ops is the (possibly empty)
    // list of image processing operations (Aliyun only).
    // The bucket parameter is part of the signature for future per-bucket
    // CDN support; current implementations ignore it (per-provider domain).
    CDNURL(ctx context.Context, bucket, objectKey string, ops []Op, ttl time.Duration) (url string, expiresAt time.Time, err error)
}
```

### Registry 查询方法

`internal/provider/storage/registry.go`:

```go
// CDNURLGeneratorForBucket returns the CDN URL generator for the bucket's
// provider, or nil if the provider doesn't support CDN (CDN config unset).
func (r *Registry) CDNURLGeneratorForBucket(bucket string) (types.CDNURLGenerator, error) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    providerName, ok := r.bucketProviders[bucket]
    if !ok {
        return nil, errBucketNotFound(bucket)
    }
    if g, ok := r.providers[providerName].(types.CDNURLGenerator); ok {
        return g, nil
    }
    return nil, nil
}
```

### Aliyun Type A 鉴权算法

`internal/provider/storage/aliyun/cdnauth/type_a.go`:

```go
// Package cdnauth implements Aliyun CDN URL signing algorithms.
// Spec: https://help.aliyun.com/zh/cdn/user-guide/type-a-signing
//
// Aliyun does NOT provide an SDK helper for CDN URL signing (the cdn-20180510
// SDK only covers management APIs). The algorithm is a simple MD5 over
// (URI + "-" + timestamp + "-" + rand + "-" + uid + "-" + privateKey), so
// we implement it locally with stdlib crypto/md5.
package cdnauth

import (
    "crypto/md5"
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "time"
)

// SignTypeA returns a CDN URL auth_key string for the given URI.
// Format: <timestamp>-<rand>-<uid>-<md5hex>
// where md5hex = md5(uri + "-" + timestamp + "-" + rand + "-" + uid + "-" + key)
func SignTypeA(uri, privateKey string, ts int64, uid string) (string, error) {
    rand, err := randString(16)
    if err != nil {
        return "", fmt.Errorf("generate rand: %w", err)
    }
    s := fmt.Sprintf("%s-%d-%s-%s-%s", uri, ts, rand, uid, privateKey)
    sum := md5.Sum([]byte(s))
    return fmt.Sprintf("%d-%s-%s-%s", ts, rand, uid, hex.EncodeToString(sum[:])), nil
}

// randString returns n hex-encoded random bytes (2n hex chars).
func randString(n int) (string, error) {
    b := make([]byte, n)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return hex.EncodeToString(b), nil
}

// ResolveExpiry returns the Unix timestamp at which the URL expires.
// Aliyun Type A auth_key has its own timestamp field; expiry = ts + maxTTL
// (typically auth_key's ts IS the expiry, set by the caller).
func ResolveExpiry(now time.Time, ttl time.Duration) int64 {
    return now.Add(ttl).Unix()
}
```

### Aliyun Provider 实现

`internal/provider/storage/aliyun/cdn.go`:

```go
package aliyun

import (
    "context"
    "fmt"
    "net/url"
    "time"

    "storage-service/internal/provider/storage/aliyun/cdnauth"
    "storage-service/internal/provider/storage/types"
)

var _ types.CDNURLGenerator = (*AliyunProvider)(nil)

// CDNURL builds an Aliyun CDN signed URL (Type A auth_key) for the object.
// x-oss-process is appended to the URL when ops is non-empty; Aliyun CDN
// transparently forwards it to OSS on cache miss.
func (p *AliyunProvider) CDNURL(_ context.Context, _, objectKey string, ops []types.Op, ttl time.Duration) (string, time.Time, error) {
    if p.cdnConfig == nil {
        return "", time.Time{}, types.ErrCDNNotConfigured
    }
    now := time.Now()
    expiresAt := now.Add(ttl)

    // Type A: auth_key path = <objectKey> (without leading /)
    authKey, err := cdnauth.SignTypeA(objectKey, p.cdnConfig.AuthKey, expiresAt.Unix(), "0")
    if err != nil {
        return "", time.Time{}, fmt.Errorf("sign cdn url: %w", err)
    }

    u := &url.URL{
        Scheme: "https",
        Host:   p.cdnConfig.Domain,
        Path:   objectKey,
    }
    q := u.Query()
    q.Set("auth_key", authKey)
    if len(ops) > 0 {
        q.Set("x-oss-process", buildOssProcessStyle(ops))
    }
    u.RawQuery = q.Encode()
    return u.String(), expiresAt, nil
}
```

### S3 Provider 实现

`internal/provider/storage/s3/cdn.go`:

```go
package s3

import (
    "context"
    "fmt"
    "net/url"
    "time"

    "storage-service/internal/provider/storage/types"

    "github.com/aws/aws-sdk-go-v2/service/cloudfront/sign"
)

var _ types.CDNURLGenerator = (*S3Provider)(nil)

// CDNURL builds a CloudFront Signed URL. S3+CloudFront does not support
// image processing at the edge; non-empty ops returns ErrCDNImageProcessingUnsupported.
func (p *S3Provider) CDNURL(_ context.Context, _, objectKey string, ops []types.Op, ttl time.Duration) (string, time.Time, error) {
    if p.cdnConfig == nil {
        return "", time.Time{}, types.ErrCDNNotConfigured
    }
    if len(ops) > 0 {
        return "", time.Time{}, types.ErrCDNImageProcessingUnsupported
    }
    now := time.Now()
    expiresAt := now.Add(ttl)

    rawURL := (&url.URL{
        Scheme: "https",
        Host:   p.cdnConfig.Domain,
        Path:   objectKey,
    }).String()

    privKey, err := sign.LoadPEMPrivKeyFile(p.cdnConfig.PrivateKeyPath)
    if err != nil {
        return "", time.Time{}, fmt.Errorf("load cloudfront private key: %w", err)
    }
    signer := sign.NewURLSigner(p.cdnConfig.KeyPairID, privKey)
    signed, err := signer.Sign(rawURL, expiresAt)
    if err != nil {
        return "", time.Time{}, fmt.Errorf("sign cloudfront url: %w", err)
    }
    return signed, expiresAt, nil
}
```

### FakeProvider 实现

```go
// CDNURL returns a placeholder CDN URL. Used by service-layer integration
// tests to exercise the full flow.
func (*FakeProvider) CDNURL(_ context.Context, _, objectKey string, ops []types.Op, ttl time.Duration) (string, time.Time, error) {
    now := time.Now()
    expiresAt := now.Add(ttl)
    u := &url.URL{
        Scheme: "https",
        Host:   "cdn.test.example",
        Path:   objectKey,
    }
    q := u.Query()
    q.Set("fake_auth", "test-signature")
    q.Set("expires", fmt.Sprintf("%d", expiresAt.Unix()))
    if len(ops) > 0 {
        q.Set("x-oss-process", "fake-style")
    }
    u.RawQuery = q.Encode()
    return u.String(), expiresAt, nil
}
```

### 构造函数签名变化

| Provider | 新签名 |
|----------|--------|
| `aliyun.NewAliyunProvider` | `(endpoint, ak, sk, roleARN, region string, cdn *CDNConfig)` |
| `s3.NewS3Provider` | `(endpoint, region, ak, sk, roleARN string, cdn *CDNConfig)` |

CDN 用 struct pointer（nil = 不启用 CDN），不用 string。

### Service 层

`internal/service/file/file.go` 加 `GetCDNURL`:

```go
func (s *Service) GetCDNURL(ctx context.Context, req *storagev1.GenerateCDNURLRequest) (*storagev1.GenerateCDNURLResponse, error) {
    ownerType := int32(req.GetOwner().GetOwnerType())
    ownerID := req.GetOwner().GetOwnerId()

    file, err := dal.GetFileByID(ctx, s.db, req.GetFileId())
    if err != nil {
        return nil, xcodes.ErrFileNotFound.Wrap(err)
    }
    if file.OwnerType != ownerType || file.OwnerID != ownerID {
        return nil, xcodes.ErrFileNotFound.New("file not owned by caller")
    }

    obj, err := dal.GetObjectByID(ctx, s.db, file.ObjectID)
    if err != nil {
        return nil, xcodes.ErrInternal.Wrap(err)
    }

    gen, err := s.registry.CDNURLGeneratorForBucket(obj.Bucket)
    if err != nil {
        return nil, xcodes.ErrBucketNotFound.Wrap(err)
    }
    if gen == nil {
        return nil, xcodes.ErrCDNNotConfigured.New("provider for bucket %q has no CDN configured", obj.Bucket)
    }

    ttl := s.resolveCDNTTL(req.GetTtl().AsDuration())

    var ops []types.Op
    for _, p := range req.GetOps() {
        ops = append(ops, conv.ProtoToImageOp(p))
    }

    url, expiresAt, err := gen.CDNURL(ctx, obj.Bucket, obj.ObjectKey, ops, ttl)
    if err != nil {
        if errors.Is(err, types.ErrCDNImageProcessingUnsupported) {
            return nil, xcodes.ErrCDNImageProcessingUnsupported.Wrap(err)
        }
        return nil, xcodes.ErrInternal.Wrap(err)
    }

    return &storagev1.GenerateCDNURLResponse{Url: url, ExpiresAt: expiresAt.Unix()}, nil
}

// resolveCDNTTL clamps ttl to [cdn.min_ttl, cdn.max_ttl]; 0 → default_ttl.
func (s *Service) resolveCDNTTL(ttl time.Duration) time.Duration {
    cfg := s.cfg.Storage.CDN
    if ttl == 0 {
        return cfg.DefaultTTL
    }
    if ttl < cfg.MinTTL {
        return cfg.MinTTL
    }
    if ttl > cfg.MaxTTL {
        return cfg.MaxTTL
    }
    return ttl
}
```

### Error Codes 新增

`pkg/xcodes/cdn.go`:

| Code | Reason | HTTP | 场景 |
|------|--------|------|------|
| `ErrCDNNotConfigured` | `CDN_NOT_CONFIGURED` | 400 | provider 没配 CDN |
| `ErrCDNImageProcessingUnsupported` | `CDN_IMAGE_PROCESSING_UNSUPPORTED` | 400 | S3 + 非空 ops |

（移除 `ErrFilePrivate` — 鉴权 URL 模式下 is_public 不再检查）

### Config 改动

`pkg/config/config.go`:

```go
type ProviderConfig struct {
    Name      string
    Vendor    string
    Endpoint  string
    Region    string
    AccessKey string
    SecretKey string
    RoleARN   string
    CDN       *CDNConfig  // nil = 不启用
    Buckets   []*BucketConfig
}

// CDNConfig configures CDN signing for a provider. Currently supports
// Aliyun Type A (MD5 auth_key) and AWS CloudFront Signed URL (RSA).
type CDNConfig struct {
    Domain    string  // cdn.example.com (bare hostname, no scheme)
    AuthType  string  // "aliyun-type-a" | "cloudfront"
    AuthKey   string  // Aliyun: 32-byte primary key (literal). CloudFront: PEM private key content OR file path.
    KeyPairID string  // CloudFront only (AuthType=cloudfront): key pair ID from AWS account
}

// CDNRuntimeConfig at Storage level — TTL defaults/limits.
type CDNRuntimeConfig struct {
    DefaultTTL time.Duration `default:"1h"`
    MinTTL     time.Duration `default:"5m"`
    MaxTTL     time.Duration `default:"24h"`
}
```

`Validate()` 加：
- `p.CDN != nil && p.CDN.Domain == ""` → error
- domain 格式校验：禁止 `http://` 前缀、禁止末尾 `/`、必须含 `.`
- `p.CDN.AuthType` ∈ `{"aliyun-type-a", "cloudfront"}`
- `p.CDN.AuthKey == ""` → error（密钥必须配）
- CloudFront 时 `p.CDN.KeyPairID == ""` → error

### Registry / newProvider 改动

`registry.newProvider` 的 Aliyun / S3 分支读 `cfg.CDN`：
- 配了 → 把 `*CDNConfig` 传给 provider 构造函数
- 没配 → 传 nil；provider 内部 `cdnConfig == nil` 时不实现接口（registry 类型断言返 nil）

## Testing

### 单元测试

| 文件 | 测试 |
|------|------|
| `aliyun/cdnauth/type_a_test.go` | `TestSignTypeA_KnownVector`（用阿里云文档示例的固定 ts/rand/uid/key 验证 MD5 哈希值）<br>`TestSignTypeA_RandomIsUnique`（不同调用 rand 不同） |
| `aliyun/cdn_test.go` | `TestAliyunProvider_CDNURL_PlainDownload`（断言 auth_key 存在、URL 格式正确）<br>`TestAliyunProvider_CDNURL_WithImageOps`（断言 x-oss-process 参数）<br>`TestAliyunProvider_CDNURL_Expiry`（断言 expiresAt = now + ttl）<br>`TestAliyunProvider_CDNURL_NoConfig` |
| `s3/cdn_test.go` | `TestS3Provider_CDNURL_PlainDownload`（用临时 RSA 私钥文件）<br>`TestS3Provider_CDNURL_ImageOpsRejected`<br>`TestS3Provider_CDNURL_NoConfig` |
| `file/file_test.go` | `TestGetCDNURL_NotOwner`<br>`TestGetCDNURL_NoCDNConfigured`<br>`TestGetCDNURL_AliyunHappyPath`<br>`TestGetCDNURL_S3ImageOpsRejected`<br>`TestGetCDNURL_DefaultTTL`（不传 ttl 用 default）<br>`TestGetCDNURL_ClampTTL`（小于 min/大于 max 被 clamp） |
| `pkg/config/config_test.go` | `TestCDNConfig_Validate_*`（domain required / format / authtype / authkey / keypairid） |

### 关键测试：Aliyun Type A MD5 已知向量

阿里云文档给了一组示例：
```
PrivateKey = aliyun_cdn_test_key
URI = /image/example.png
Timestamp = 1511995199
Rand = rand
Uid = userid
Expected auth_key MD5 = ...
```

`TestSignTypeA_KnownVector` 用这些固定输入，断言 `md5(uri-timestamp-rand-uid-key)` 的 hex 输出跟文档示例完全一致。这锁定了算法实现。

### 集成测试

不跑真 CDN — CDN URL 是纯字符串拼接 + 本地签名计算，无网络调用，单元测试足够。FakeProvider 实现 `CDNURLGenerator` 让 service 层测试覆盖完整流程。

### 不在本 spec 范围内的测试

- 真实 CDN 节点验签行为（依赖 CDN 控制台配置）
- Aliyun Type B/C/D（同构，未实现）
- CloudFront Signed Cookie
- Lambda@Edge 边缘图片处理

## Acceptance Criteria

1. ✅ `GenerateCDNURL(file_id, ops=[])` 对配了 CDN 的 Aliyun provider 返回 `https://<domain>/<key>?auth_key=...`
2. ✅ `GenerateCDNURL(file_id, ops=[resize])` 对 Aliyun 返回带 `auth_key` + `x-oss-process` 的 URL
3. ✅ 同上对 S3 返回 `CDN_IMAGE_PROCESSING_UNSUPPORTED`
4. ✅ `GenerateCDNURL` 对没配 CDN 的 provider 返回 `CDN_NOT_CONFIGURED`
5. ✅ 不属于 caller 的 file_id 返回 `FILE_NOT_FOUND`（不泄露存在性）
6. ✅ URL 含 `expires_at` 字段，值 = now + ttl
7. ✅ 不传 ttl 时用 default_ttl（1h），超出 [min, max] 被夹紧
8. ✅ `ProviderConfig.CDN.AuthType` / `AuthKey` / `Domain` 错误 → 启动时 `Validate` 失败
9. ✅ Aliyun Type A MD5 算法用文档已知向量验证（`TestSignTypeA_KnownVector`）
10. ✅ FakeProvider 实现 CDNURLGenerator，service 层集成测试覆盖完整流程

## Migration / Compatibility

- proto 加 RPC 是非破坏性（旧客户端不调用 = 无影响）
- `ProviderConfig.CDN` 是可选字段，nil = 现状（不启用 CDN）
- `aliyun.NewAliyunProvider` / `s3.NewS3Provider` 签名变化（加 `*CDNConfig` 参数），但只在 `registry.go` 和 provider 自己的 test 里调用
- 现有 `GenerateDownloadURL` / `GenerateProcessURL` 完全不动
- 现有 `StorageFile.is_public` 字段保留（其他地方用），CDN URL 不再检查它

## Out of Scope

- Aliyun Type B/C/D 鉴权（同构 Type A，未来按需补）
- CloudFront Signed Cookie
- Lambda@Edge / CloudFront Functions 边缘图片处理
- CDN URL 鉴权密钥轮换（运维侧在 CDN 控制台配置）
- CDN 缓存预热 / 刷新 API（PushObjectCache / RefreshObjectCaches）

## Risks

1. **密钥管理** — CDN 鉴权密钥（Aliyun primary key / CloudFront RSA 私钥）写在 config.yaml 里。建议走环境变量 / Secret Manager 注入，不在仓库明文。
2. **Type A 已知向量** — 必须用阿里云文档示例验证 MD5 输出，否则上线后 CDN 节点验签失败。
3. **TTL 配置上限** — Aliyun Type A 实际上不强制 maxTTL（CDN 控制台可以设），但我们代码层夹紧到 24h 避免无限 URL。
4. **x-oss-process 缓存键** — 不同 ops 组合 = 不同缓存项。客户端应使用有限的预设组合（业务侧约定，不在代码层强制）。
5. **CloudFront 私钥 PEM 文件路径** — `AuthKey` 字段对 CloudFront 是文件路径不是密钥内容。两种 vendor 用同一字段名但语义不同，doc comment 要明确。
