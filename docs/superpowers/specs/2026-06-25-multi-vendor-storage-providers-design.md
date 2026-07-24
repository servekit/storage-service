# 多 Vendor 存储扩展 Design

- **Date**: 2026-06-25
- **Status**: Approved (brainstorming complete, ready for plan)
- **Author**: storage-service team

## 背景与目标

storage-service 当前的 vendor 抽象层支持 `VENDOR_ALIYUN_OSS` / `VENDOR_AWS_S3` / `VENDOR_S3_COMPATIBLE`，覆盖阿里云 OSS、AWS S3、MinIO 等 S3 兼容存储。

本 design 扩展三家国内主流云厂商的对象存储 + CDN 接入：

- **腾讯云 COS**（Cloud Object Storage）
- **华为云 OBS**（Object Storage Service）
- **火山引擎 TOS**（Tinder Object Storage）

目标：在不动现有 vendor 代码、不破坏现有部署的前提下，让 service 在 yaml 加一段配置即可对接任一新 vendor。

## 决策摘要

| 决策点 | 选择 |
|--------|------|
| 火山引擎 SDK 路径 | 原生 TOS SDK (`ve-tos-golang-sdk/v2` v2.9.6) |
| 实施 PR 划分 | Phase 0 架构 PR + Phase 1 三家并行 PR |
| CDN 签名覆盖 | 每个 vendor 只实现 Type A |
| 图片处理 builder | 每 vendor 独立 helper（不抽公共） |
| STS PolicyBuilder | 每 vendor 独立实现（JSON 语法各家不兼容） |
| STS RoleARN 字段 | 保持 `string` 类型不变，per-vendor 语义注释 + 运行时校验 |
| **CDNConfig.AuthType 字段** | **删除**（运行时未使用，vendor 已是 source of truth；YAGNI） |
| 测试策略 | Unit test 为主，集成测试带 build tag 后续补 |
| Vendor enum 命名 | `VENDOR_TENCENT_COS` / `VENDOR_HUAWEI_OBS` / `VENDOR_VOLCENGINE_TOS` |

## 总体架构

### 包布局

新增三个 vendor 包，与现有 `aliyun/` / `s3/` / `fake/` 平级：

```
internal/provider/storage/
├── aliyun/                  (现有)
├── s3/                      (现有)
├── fake/                    (现有)
├── tencent/                 (新)
│   ├── provider.go          # *TencentProvider 实现 types.Provider 8 方法
│   ├── cdn.go               # *CDNURLGenerator + signTencentTypeA + buildTencentStyle
│   ├── sts.go               # STS client + PolicyBuilder
│   ├── image.go             # buildTencentStyle helper (imageMogr2)
│   └── *_test.go
├── huawei/                  (新,同构)
├── volcengine/              (新,同构)
├── registry.go              # 扩 newProvider / newCDNURLGenerator
└── types/
    └── cdn.go               # CDNURLOptions 不变
```

每个 vendor 包文件布局严格对齐 `aliyun` 包：`provider.go` 在顶部、`cdn.go` / `sts.go` / `image.go` 在中间、helpers 在底。

### Phase 0:架构 PR(先合并)

单 PR,不依赖任何 vendor 代码:

1. Proto Vendor enum 扩展(加 4/5/6 字段)
2. **删除 `CDNConfig.AuthType` 字段**(详见下文 "Schema 变更" 节):
   - 字段从 struct 移除
   - `validateBucketCDN` 删除 AuthType 校验,改为按 vendor 强制 `KeyPairID`(cloudfront 路径需要)
   - `config_test.go` 删/改相关测试
   - `config.example.yaml` 移除所有 `auth_type:` 行
   - `aliyun/cdn_test.go`、`s3/cdn_test.go` 删除 `AuthType: ...` 测试夹具
3. `ProviderConfig.RoleARN` 注释扩 per-vendor 语义
4. Registry 占位扩展:`newProvider` / `newCDNURLGenerator` 加 case,返回 "unsupported vendor" 错误
5. `config.example.yaml` 加三家 vendor 注释示例

**Phase 0 合并后,主分支编译通过、所有现有测试通过**——只是新 vendor 还没实现,配了会报 "unsupported vendor"。

### Phase 1:三个 vendor PR(并行启动)

| PR | 内容 |
|----|------|
| PR-tencent | `tencent/` 整包 + Registry 接线 |
| PR-huawei | `huawei/` 整包 + Registry 接线 + IAM PolicyBuilder |
| PR-volcengine | `volcengine/` 整包 + Registry 接线 |

腾讯 PR 作为参照实现先完成,huawei/volcengine PR 沿用同一骨架。

**回滚边界**:每个 vendor PR 独立可 revert,不影响其他 vendor。

## Schema 扩展

### Vendor enum(proto)

```protobuf
enum Vendor {
  VENDOR_UNSPECIFIED = 0;
  VENDOR_ALIYUN_OSS = 1;
  VENDOR_AWS_S3 = 2;
  VENDOR_S3_COMPATIBLE = 3;
  VENDOR_TENCENT_COS = 4;       // 新
  VENDOR_HUAWEI_OBS = 5;        // 新
  VENDOR_VOLCENGINE_TOS = 6;    // 新
}
```

字段编号一旦发布不可改,1/2/3 保持不变。

### CDNConfig.AuthType 字段:删除

经审阅,`AuthType` 字段在 provider 运行时**完全未被读取**:

- `aliyun/cdn.go`、`s3/cdn.go` 都不读 `AuthType`——只读 `Domain` / `AuthKey` / `KeyPairID`
- `registry.go:newCDNURLGenerator` 用 **vendor**(不是 AuthType)选择 generator 类型
- `AuthType` 唯一被消费的地方是 `validateBucketCDN`——做 vendor↔AuthType 一致性校验,但 vendor↔AuthType 一一对应(同义反复)

**结论**:YAGNI。删除字段,vendor 直接是 source of truth。

删除后 `CDNConfig` 简化为:

```go
type CDNConfig struct {
    Domain    string
    AuthKey   string
    KeyPairID string // 仅 cloudfront 路径(AWS_S3 / S3_COMPATIBLE)需要
}
```

`validateBucketCDN` 改为按 vendor 强制 KeyPairID:

```go
switch vendor {
case "VENDOR_AWS_S3", "VENDOR_S3_COMPATIBLE":
    if cdn.KeyPairID == "" {
        return error("key_pair_id required for cloudfront")
    }
default:
    // 其他 vendor 不用 KeyPairID,允许为空
}
```

**影响范围**:
- `pkg/config/config.go`:删除字段 + 改 Validate
- `pkg/config/config_test.go`:删除所有 `AuthType: ...` 测试夹具
- `config.example.yaml`:移除所有 `auth_type:` 行
- `internal/provider/storage/aliyun/cdn_test.go`:删除 `AuthType: "aliyun-type-a"`
- `internal/provider/storage/s3/cdn_test.go`:删除 `AuthType: "cloudfront"`

未来如果某 vendor 真的支持多种签名算法(如腾讯 Type B/C/D),再恢复一个 per-vendor 的算法选择字段——但那是真有诉求时的事。

### ProviderConfig.RoleARN per-vendor 语义

字段类型不变(`string`),注释和运行时校验按 vendor 分流:

| Vendor | RoleARN 格式 | STS 是否需要 |
|--------|-------------|------------|
| VENDOR_ALIYUN_OSS | `acs:ram::<account-id>:role/<role-name>` | 必填 |
| VENDOR_AWS_S3 | `arn:aws:iam::<account-id>:role/<role-name>` | 必填 |
| VENDOR_S3_COMPATIBLE | any non-empty | 可选(MinIO 不校验) |
| VENDOR_TENCENT_COS | UNUSED(CAM STS 不用 RoleARN) | 留空 |
| VENDOR_HUAWEI_OBS | agency name(委托名) | 必填 |
| VENDOR_VOLCENGINE_TOS | `trn:iam::<account-id>:role/<role-name>` | 必填 |

**校验时机**:不在 `Validate()` 强制(STS 是 opt-in)。让 vendor 包的 `NewXxxProvider` 在 RoleARN 非空但格式错误时返回 error(运行时 fail-fast)。

### BucketConfig

**完全不动**。现有 `Name` / `KeyPrefix` / `ACL` / `CDN` 字段足够覆盖三家。腾讯云 bucket 名带 APPID 后缀的限制在 `config.example.yaml` 文档说明,不强制 schema 校验。

## 每 vendor 实现要点

### Tencent COS

**SDK**:`github.com/tencentyun/cos-go-sdk-v5` v0.7.74
**STS SDK**:`github.com/tencentyun/qcloud-cos-sts-sdk`(无 semver,go.mod pin commit)
**Endpoint**:`https://<bucket-appid>.cos.<region>.myqcloud.com`

| Provider 方法 | 调用 |
|--------------|------|
| PutObject | `c.Object.Put(ctx, key, reader, opt)` |
| GetObject | `c.Object.Get(ctx, key, nil)` |
| DeleteObject | `c.Object.Delete(ctx, key)` |
| HeadObject | `c.Object.Head` + `c.Object.GetACL`(ACL 兜底,跟 aliyun 一致) |
| ListObjects | `c.Bucket.Get(&cos.BucketGetOptions{Prefix})` |
| PresignPutObject | `c.Object.GetPresignedURL(ctx, PUT, key, ak, sk, ttl, opt)` |
| PresignGetObject | 同上换 GET,opt.Query 可塞 response-content-disposition |
| GetSTSToken | `sts.New(...).GetCredential(opt)`,policy 自拼 JSON |

**图片处理 builder**:`buildTencentStyle(ops) → imageMogr2/thumbnail/100x100/format/webp`(vendor-specific,跟 aliyun 完全不同)

**CDN Type A**:`signTencentTypeA(uri, key, ts) → md5(uri-timestamp-rand-uid-key)`(公式跟 aliyun 相同,独立实现)

**Tencent STS Policy JSON**:
```json
{
  "version": "2.0",
  "statement": [{
    "effect": "allow",
    "action": ["name/cos:PostObject", "name/cos:PutObject"],
    "resource": ["qcs::cos:<region>:uid/<appid>:<bucket-appid>/<prefix>/*"]
  }]
}
```

### Huawei OBS

**SDK**:`github.com/huaweicloud/huaweicloud-sdk-go-obs` v3.26.3
**IAM SDK**:`github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam`(多一个依赖)
**Endpoint**:`obs.<region>.myhuaweicloud.com`

| Provider 方法 | 调用 |
|--------------|------|
| PutObject | `client.PutObject(&obs.PutObjectInput{...})` |
| GetObject | `client.GetObject(...)` |
| DeleteObject | `client.DeleteObject(...)` |
| HeadObject | `client.HeadObject` + `client.GetObjectAcl`(ACL 兜底) |
| ListObjects | `client.ListObjects(...)` 分页 |
| PresignPutObject | `client.CreateBrowserPresignedUrl(&input{Method: PUT, ...})` |
| PresignGetObject | 同上换 GET |
| GetSTSToken | `IamClient.CreateTemporaryAccessKeyByAgency(req)` |

**图片处理 builder**:`buildHuaweiStyle(ops) → image/resize,p_100`(跟 aliyun 几乎一致,prefix 不同)

**CDN Type A**:`signHuaweiTypeA(uri, key, ts) → md5(uri-timestamp-rand-uid-key)`(公式跟 aliyun 完全相同)

**Huawei IAM Policy JSON**(vendor-specific,最复杂):
```json
{
  "Version": "1.1",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["obs:object:PutObject"],
    "Resource": ["OBS:*:*:object:<bucket>/<prefix>/*"]
  }]
}
```

华为 policy builder 是**最大工作量**——语法跟 aliyun/AWS 不兼容,需要独立写。

### Volcengine TOS

**SDK**:`github.com/volcengine/ve-tos-golang-sdk/v2` v2.9.6
**STS SDK**:`github.com/volcengine/volcengine-go-sdk` v1.2.36(service/sts)
**Endpoint**:`tos-cn-beijing.volces.com`(原生,不走 S3 兼容路径)

| Provider 方法 | 调用 |
|--------------|------|
| PutObject | `client.PutObjectV2(ctx, &tos.PutObjectV2Input{...})` |
| GetObject | `client.GetObjectV2(ctx, ...)` |
| DeleteObject | `client.DeleteObjectV2(...)` |
| HeadObject | `client.HeadObjectV2(...)` |
| ListObjects | `client.ListObjectsType2(...)` 分页 |
| PresignPutObject | `client.PreSignedURL(&tos.PreSignedURLInput{HTTPMethod: PUT, ...})` |
| PresignGetObject | 同上换 GET,可塞 Query;原生支持 CDN 域名替换(`AlternativeEndpoint`/`IsCustomDomain`) |
| GetSTSToken | `sts.New(...).AssumeRole(ctx, &AssumeRoleInput{RoleTrn, ...})` |

**图片处理 builder**:`buildVolcStyle(ops) → image/resize,w_100,h_100`(跟 aliyun 一致)

**CDN Type A**:`signVolcTypeA(uri, key, ts) → md5(uri-timestamp-rand-uid-key)`

**Volcengine STS Policy JSON**:
```json
{
  "Statement": [{
    "Effect": "Allow",
    "Action": ["tos:PutObject"],
    "Resource": ["trn:tos:::<bucket>/<prefix>/*"]
  }]
}
```

### 跨 vendor 一致性约束

每个 vendor PR 必须满足:

1. **Provider 接口完整实现**——8 方法 + `types.CDNURLGenerator`(独立 generator 类型)
2. **HeadObject 不返回 ACL 的兜底**——aliyun 模式:HeadObject + 单独 GetObjectAcl,best-effort(失败时 ObjectACL 留空)
3. **CDN Type A known vector 测试**——每个 vendor 至少 1 个,用官方文档固定输入校验
4. **STS PolicyBuilder 单元测试**——固定 policy 输入断言 JSON 输出
5. **KeyPairID 必填性按 vendor 兜底**——已由 `validateBucketCDN` 在 Phase 0 处理(cloudfront 路径强制,其他 vendor 禁用)

### 三家工作量预估

| Vendor | 估算代码量 | 难点 |
|--------|-----------|------|
| Tencent | ~430 行 | imageMogr2 语法差异 / STS Policy JSON builder |
| Huawei | ~350 行 | **2 个 SDK 依赖** / IAM Policy builder 最复杂 |
| Volcengine | ~300 行 | TOS SDK 较新,可能遇到边角问题 |

## 测试策略

### 测试文件布局

每个 vendor 包:

```
tencent/
├── cdn_test.go         # CDN Type A known vector + generator 行为
├── provider_test.go    # 8 个 Provider 方法的 mock 调用验证
├── sts_test.go         # PolicyBuilder JSON 输出 + GetSTSToken 错误路径
└── image_test.go       # buildTencentStyle 输出格式
```

`huawei/` / `volcengine/` 同布局。

### CDN known vector 测试(强制)

每个 vendor 的 `cdn_test.go` 必须有官方文档固定输入测试,防算法漂移:

- Tencent Type A: cloud.tencent.com/document/product/228/41623
- Huawei Type A: support.huaweicloud.com/usermanual-cdn/cdn_01_0040.html
- Volcengine Type A: www.volcengine.com/docs/6454/1129831

### Provider 方法测试(mock SDK)

统一用 `httptest.Server` mock HTTP,验证 provider 转发的 URL/header/params。不依赖官方 mock helper。

最小覆盖:
- PutObject / GetObject / DeleteObject / HeadObject / ListObjects 各 1 个 happy path
- PresignPutObject / PresignGetObject 验证返回 URL 形态
- HeadObject 不返回 ACL 时调 GetObjectACL 的兜底

### STS PolicyBuilder 测试

两个层面:

1. PolicyBuilder 输出(不用 mock SDK):`assert.JSONEq` 验证 JSON
2. GetSTSToken 错误路径:无 RoleARN / nil client

### fake 包

**不动**。现有 `fake.CDNURLGenerator` / `fake.FakeProvider` 是 vendor-neutral 的,service 层测试不需要新 vendor 的 fake。

### Registry 测试扩展

加 vendor 路由测试:

- `TestNewRegistry_VendorDispatch`:5 个 vendor 各自分发到正确 provider 类型
- `TestNewCDNURLGenerator_VendorDispatch`:vendor ↔ generator 类型匹配

### 集成测试(不在本次范围)

按既定决策("Unit 为主,集成后续补"):

- 本期所有 PR **不阻塞合并**于集成测试缺失
- 在 `docs/deployment/` 新建三份文档:
  - `tencent-smoke-test.md`
  - `huawei-smoke-test.md`
  - `volcengine-smoke-test.md`
- 提供:云上 dev 账号准备、`go test -tags=integration ./internal/provider/storage/<vendor>/ -run Integration` 命令、校验 checklist
- 集成测试代码带 build tag,不进默认 `go test ./...`

### CI 影响

- 现有 CI(`go test -race ./...`)跑所有 unit test——三个 vendor 包都进 CI
- 新增 5 个 SDK 依赖,编译时间增加,但 unit test 影响有限(mock server)
- 不动现有 presign testcontainer 测试

## 风险

本项目当前没有外部用户依赖 yaml schema,可以随意 breaking。本节只列依赖/实施风险。

### 依赖风险

| SDK | 风险 | 缓解 |
|------|------|------|
| Tencent `cos-go-sdk-v5` v0.7.74 | v0.x,活跃维护 | 锁版本 |
| Tencent STS(无 semver) | 中等 | go.mod pin commit + 注释 |
| Huawei OBS v3.26.3 | 稳定,无 v2/v3 过渡 | 锁版本 |
| Huawei IAM v3 | 主仓库大 | 只引 `services/iam` 子模块 |
| Volcengine TOS v2.9.6 | 半年内 v2.7→v2.9 | 锁版本,留意 BREAKING CHANGELOG |
| Volcengine STS v1.2.36 | 代码生成,不能 fork | 直接 import |

go.mod 总依赖膨胀预估 vendor 后 ~50MB(阿里 + AWS 已 ~30MB),可接受。

### 已知限制 / 待办(本期不做)

1. **Tencent/Huawei/Volcengine CDN Type B/C/D 等**——只 Type A
2. **Volcengine veImageX**——原生 TOS 图片处理够用
3. **STS Policy size/MD5 限制**——三家 SDK 都不支持,跟阿里/AWS 一样靠客户端 PostObject 表单兜底
4. **Object ACL HeadObject 不返回**——三家都需 GetACL 兜底

## 部署 Checklist(每 vendor 一份)

### Tencent 特有

- bucket 名必须含 APPID 后缀(`<name>-<appid>`)
- STS 时长上限 7200s(比阿里云 3600 长,比 AWS 43200 短)——STS 缓存 TTL 配置需复核
- 2022-05-09 后新 bucket 不提供"默认 CDN 加速域名",必须配 ICP 备案的自定义域名

### Huawei 特有

- 需在 IAM 控制台预创建 Agency(委托),AgencyName 填 RoleARN
- IAM Policy 是华为自有 JSON 语法,不能复用阿里/AWS 模板
- OBS 内置图片处理走 `x-image-process` header / query

### Volcengine 特有

- RoleARN 用 TRN 格式(`trn:iam::<id>:role/<name>`),跟阿里 ARN 格式不同但都是字符串
- 信任策略需把 `trn:iam::<id>:root` 改成具体 IAM user
- TOS 原生图片处理参数透传 CDN,CDN 不参与处理

### 共享(已在 `config.example.yaml` 注释)

- CDN cache key 排除 `auth_key` / `Signature` / `Expires` / `Key-Pair-Id`
- `response-content-disposition` 必须从 cache key 排除 + 转发给 origin(CloudFront 需配 Origin Request Policy)

## Follow-up(明确不在本期)

- 集成测试(带 `-tags=integration` build tag,需云上 dev 账号)
- 其他 CDN 签名算法(Type B/C/D 等)支持——未来如果某 vendor 真需要多种算法,届时再加 per-vendor 算法选择字段
- veImageX / Tencent CI 高级能力
- 监控/告警 vendor-specific 指标(STS 失败率、CDN 命中率等)

## 风险登记表

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| Tencent STS SDK 无 semver | 低 | 中 | go.mod pin commit + 注释 + 升级时手测 |
| Volcengine TOS SDK 较新 | 中 | 中 | PR-tencent 跑通后,PR-volcengine 留充分 smoke test 时间 |
| Huawei 2 个 SDK 依赖管理冲突 | 低 | 低 | `go mod tidy` 后跑全量测试 |
| CDN Type A 算法各家细节差异 | 中 | 高 | known vector 测试强制覆盖 |
| 火山云 STS 信任策略配错 | 中 | 中 | 部署文档详细说明 + smoke-test 脚本 |

## 参考文档

### Tencent
- COS Go SDK: github.com/tencentyun/cos-go-sdk-v5
- 预签名 URL: cloud.tencent.com/document/product/436/35059
- STS SDK: github.com/tencentyun/qcloud-cos-sts-sdk
- CDN Type A 鉴权: cloud.tencent.com/document/product/228/41623

### Huawei
- OBS Go SDK: github.com/huaweicloud/huaweicloud-sdk-go-obs
- OBS PreSigned URL: support.huaweicloud.com/sdk-go-devg-obs/obs_33_0601.html
- IAM 临时凭证: support.huaweicloud.com/intl/zh-cn/usermanual-iam5/iam_01_1236.html
- CDN 鉴权方式 A: support.huaweicloud.com/usermanual-cdn/cdn_01_0040.html

### Volcengine
- TOS Go SDK: github.com/volcengine/ve-tos-golang-sdk
- TOS PreSignedURL: www.volcengine.com/docs/6349/93477
- STS 临时 AK/SK: www.volcengine.com/docs/6349/127695
- CDN URL 鉴权: www.volcengine.com/docs/6454/99849
- CDN Type A: www.volcengine.com/docs/6454/1129831
