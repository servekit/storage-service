# S3 Provider STS Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the stub `S3Provider.GetSTSToken` with a real implementation that mints STS credentials for AWS S3 and MinIO/S3-compatible backends, matching the Aliyun provider's hardening (HTTPS-only / ACL lock / Deny PutObjectAcl).

**Architecture:** New `internal/provider/storage/s3/sts.go` mirrors the structure of `internal/provider/storage/aliyun/sts.go` — types, constructor, `GetSTSToken` method, `buildS3Policy` translator. STS endpoint is derived from the existing `Endpoint` field (empty → AWS regional STS; non-empty → reuse as MinIO endpoint). `NewS3Provider` gains a `roleARN` parameter; `RoleARN == ""` means STS unavailable.

**Tech Stack:** `github.com/aws/aws-sdk-go-v2/service/sts@v1.43.2` (already transitive dep), `github.com/aws/aws-sdk-go-v2/credentials`, `github.com/aws/aws-sdk-go-v2/aws`, `net/http/httptest` for mocking.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/provider/storage/s3/sts.go` (new) | STS types, constructor, `GetSTSToken`, `buildS3Policy`, helpers |
| `internal/provider/storage/s3/sts_test.go` (new) | Unit tests mirroring `aliyun/sts_test.go` |
| `internal/provider/storage/s3/provider.go` (modify) | Add `stsCli` + `roleARN` fields to `S3Provider`; extend `NewS3Provider` signature; delete stub `GetSTSToken` |
| `internal/provider/storage/s3/provider_test.go` (modify) | Update `NewS3Provider` callers to 5-arg form |
| `internal/provider/storage/registry.go` (modify) | Pass `cfg.RoleARN` to `NewS3Provider` |
| `internal/service/upload/upload.go` (modify) | `issueUploadCredential` picks `oss:` vs `s3:` Action prefix by vendor |
| `pkg/config/config.go` (modify) | Update `RoleARN` comment (no longer Aliyun-only) |

---

## Task 1: Add `roleARN` to `S3Provider` and `NewS3Provider` (refactor)

Refactor only — no STS behavior yet. Existing tests must keep passing (the stub `GetSTSToken` stays where it is until Task 6).

**Files:**
- Modify: `internal/provider/storage/s3/provider.go:24-52`
- Modify: `internal/provider/storage/s3/provider_test.go:44,217,231`
- Modify: `internal/provider/storage/registry.go:207`

- [ ] **Step 1: Update `S3Provider` struct and `NewS3Provider` signature**

In `internal/provider/storage/s3/provider.go`:

```go
// S3Provider implements storage.Provider for AWS S3 and S3-compatible backends
// (MinIO, Ceph RGW, LocalStack). STS is optional — requires RoleARN at
// construction time, otherwise GetSTSToken returns "not configured".
type S3Provider struct {
    client    *awss3.Client
    presigner *awss3.PresignClient
    stsCli    assumeRoleCaller // nil when RoleARN == ""; GetSTSToken returns "not configured"
    roleARN   string
    region    string
    endpoint  string
}

// NewS3Provider creates a new S3Provider with static credentials. roleArn
// enables STS via GetSTSToken when non-empty — AWS format
// `arn:aws:iam::<account-id>:role/<name>`; MinIO accepts any non-empty
// identifier. Empty roleArn = STS unavailable; callers must use
// GenerateUploadURL instead.
func NewS3Provider(endpoint, region, accessKey, secretKey, roleArn string) (*S3Provider, error) {
    creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

    var opts []func(*awss3.Options)
    if endpoint != "" {
        opts = append(opts, func(o *awss3.Options) {
            o.BaseEndpoint = aws.String(endpoint)
            o.UsePathStyle = true
        })
    }

    client := awss3.New(awss3.Options{
        Region:      region,
        Credentials: creds,
    }, opts...)

    p := &S3Provider{
        client:    client,
        presigner: awss3.NewPresignClient(client),
        roleARN:   roleArn,
        region:    region,
        endpoint:  endpoint,
    }

    // STS client init deferred to Task 2 (newSTSClient doesn't exist yet).
    // This task only changes the struct/constructor signature; existing
    // GetSTSToken stub stays in place.

    return p, nil
}
```

**Note:** `stsCli` field is declared but unused in this task — Go will complain. To keep this task compiling in isolation, either:
- (a) Add `var _ assumeRoleCaller = (*stsClient)(nil)` placeholder once Task 2 lands; for now comment out the field with `// stsCli assumeRoleCaller` and uncomment in Task 2, OR
- (b) Inline the field but accept that Task 2 must land before tests pass.

Recommended: option (a). Replace the struct field line with:
```go
    // stsCli assumeRoleCaller // added in Task 2
```

- [ ] **Step 2: Update `provider_test.go` callers to 5-arg form**

Three call sites in `internal/provider/storage/s3/provider_test.go` (lines 44, 217, 231). Add empty `""` roleARN:

```go
provider, err := NewS3Provider(endpoint, testRegion, testAccessKey, testSecretKey, "")
```

- [ ] **Step 3: Update `registry.go` caller**

In `internal/provider/storage/registry.go:207`:

```go
p, err := s3.NewS3Provider(cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN)
```

- [ ] **Step 4: Verify build + tests pass**

```bash
GOPROXY=https://goproxy.cn,direct go build ./...
GOPROXY=https://goproxy.cn,direct go test ./internal/provider/storage/s3/... -run 'TestS3Provider_PutAndGet|TestS3Provider_HeadObject' 2>&1 | tail -5
```

Expected: `ok` (testcontainer S3 tests still fail pre-existing on environment, but the build tests pass).

- [ ] **Step 5: Commit**

```bash
git add internal/provider/storage/s3/provider.go internal/provider/storage/s3/provider_test.go internal/provider/storage/registry.go
git commit -m "refactor(s3): add roleARN param to NewS3Provider + struct fields"
```

---

## Task 2: Create `sts.go` skeleton with types and constructor

Creates the file with types, the constant, and the `newSTSClient` constructor. No `assumeRole` method yet (Task 4) and no `GetSTSToken` (Task 6).

**Files:**
- Create: `internal/provider/storage/s3/sts.go`
- Modify: `internal/provider/storage/s3/provider.go` (un-comment the STS init block from Task 1)

- [ ] **Step 1: Create `sts.go` with types + constructor**

`internal/provider/storage/s3/sts.go`:

```go
package s3

import (
    "context"
    "fmt"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/credentials"
    awssts "github.com/aws/aws-sdk-go-v2/service/sts"
)

// stsClient wraps the AWS STS SDK so the rest of the s3 package can issue
// AssumeRole calls without exposing SDK types to callers.
type stsClient struct {
    cli *awssts.Client
}

// stsClientOpts configures newSTSClient. Endpoint empty → AWS regional STS
// (sts.<Region>.amazonaws.com); non-empty → MinIO/custom STS endpoint.
type stsClientOpts struct {
    AccessKey string
    SecretKey string
    Region    string
    Endpoint  string
}

// assumeRoleReq is the project-typed input for AssumeRole. DurationSeconds is
// int64 to match the SDK field type (AWS accepts 900..43200).
type assumeRoleReq struct {
    RoleArn         string
    RoleSessionName string
    DurationSeconds *int64
    Policy          map[string]any
}

// assumeRoleResp carries the temporary credentials. Expiration is the raw
// ISO8601 string from AWS STS; callers parse it to time.Time.
type assumeRoleResp struct {
    AccessKeyId     string
    AccessKeySecret string
    SecurityToken   string
    Expiration      string
}

// assumeRoleCaller is the contract stsClient satisfies. Tests inject a fake.
type assumeRoleCaller interface {
    assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error)
}

const (
    // minAWSSTSDuration is the lower bound AWS STS enforces on
    // DurationSeconds. Fail fast below this so callers get an actionable error
    // instead of an opaque SDK API failure.
    minAWSSTSDuration int64 = 900
)

// newSTSClient builds an AWS STS SDK client. Empty endpoint → AWS regional
// STS endpoint derived from region; non-empty → custom (MinIO/S3-compat).
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
    if opts == nil {
        return nil, fmt.Errorf("nil sts client opts")
    }
    creds := credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, "")

    var stsOpts []func(*awssts.Options)
    if opts.Endpoint != "" {
        stsOpts = append(stsOpts, func(o *awssts.Options) {
            o.BaseEndpoint = aws.String(opts.Endpoint)
        })
    }

    cli := awssts.New(awssts.Options{
        Region:      opts.Region,
        Credentials: creds,
    }, stsOpts...)
    return &stsClient{cli: cli}, nil
}
```

Note: AWS SDK v2 honors `BaseEndpoint` for redirecting to custom URLs (httptest, MinIO). No custom HTTP client needed — the default works with `http://127.0.0.1:port`.

- [ ] **Step 2: Write the failing test for `newSTSClient`**

Create `internal/provider/storage/s3/sts_test.go`:

```go
package s3

import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

// TestNewSTSClient_NilOpts verifies the constructor fails fast on nil opts.
func TestNewSTSClient_NilOpts(t *testing.T) {
    _, err := newSTSClient(nil)
    require.Error(t, err)
    assert.Contains(t, err.Error(), "nil sts client opts")
}

// TestNewSTSClient_AWSEndpoint verifies empty Endpoint leaves BaseEndpoint
// unset (AWS SDK falls back to sts.<region>.amazonaws.com).
func TestNewSTSClient_AWSEndpoint(t *testing.T) {
    c, err := newSTSClient(&stsClientOpts{
        AccessKey: "ak",
        SecretKey: "sk",
        Region:    "us-east-1",
    })
    require.NoError(t, err)
    require.NotNil(t, c)
    require.NotNil(t, c.cli)
}

// TestNewSTSClient_CustomEndpoint verifies non-empty Endpoint wires through
// to BaseEndpoint (MinIO mode).
func TestNewSTSClient_CustomEndpoint(t *testing.T) {
    c, err := newSTSClient(&stsClientOpts{
        AccessKey: "ak",
        SecretKey: "sk",
        Region:    "us-east-1",
        Endpoint:  "http://localhost:9000",
    })
    require.NoError(t, err)
    require.NotNil(t, c)
}
```

- [ ] **Step 3: Run tests**

```bash
GOPROXY=https://goproxy.cn,direct go test -v -run 'TestNewSTSClient' ./internal/provider/storage/s3/...
```

Expected: 3 PASS.

- [ ] **Step 4: Un-comment the `stsCli` field + wire STS init in `NewS3Provider`**

In `internal/provider/storage/s3/provider.go`:
1. Uncomment the `stsCli assumeRoleCaller` field in `S3Provider` struct.
2. Replace the placeholder comment in `NewS3Provider` with the real init:

```go
    if roleArn != "" {
        stsCli, err := newSTSClient(&stsClientOpts{
            AccessKey: accessKey,
            SecretKey: secretKey,
            Region:    region,
            Endpoint:  endpoint,
        })
        if err != nil {
            return nil, fmt.Errorf("init sts client: %w", err)
        }
        p.stsCli = stsCli
    }
```

- [ ] **Step 5: Verify build**

```bash
GOPROXY=https://goproxy.cn,direct go build ./...
```

Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add internal/provider/storage/s3/sts.go internal/provider/storage/s3/sts_test.go internal/provider/storage/s3/provider.go
git commit -m "feat(s3): add stsClient wrapper + newSTSClient constructor"
```

---

## Task 3: Implement `buildS3Policy` translator

Translates `*types.STSPolicy` to AWS IAM policy JSON structure. Mirrors `buildAliyunPolicy` minus region/account scoping (S3 ARNs have neither).

**Files:**
- Modify: `internal/provider/storage/s3/sts.go` (append helper at bottom)
- Modify: `internal/provider/storage/s3/sts_test.go` (append policy tests)

- [ ] **Step 1: Write failing tests for `buildS3Policy`**

Append to `internal/provider/storage/s3/sts_test.go`:

```go
import (
    // add to existing import block:
    "strings"
    "storage-service/internal/provider/storage/types"
)

// TestBuildS3Policy_NoExtensions verifies empty AllowedExtensions yields a
// single Resource wildcard covering the entire prefix. S3 ARN has no
// region/account segment (unlike Aliyun).
func TestBuildS3Policy_NoExtensions(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:    "photos",
        KeyPrefix: "uploads/",
    })
    require.NoError(t, err)

    assert.Equal(t, "2012-10-17", policy["Version"])
    stmts := policy["Statement"].([]map[string]any)
    require.Len(t, stmts, 1)
    assert.Equal(t, "Allow", stmts[0]["Effect"])
    assert.Equal(t, []string{"s3:PutObject"}, stmts[0]["Action"])
    assert.Equal(t, []string{"arn:aws:s3:::photos/uploads/*"}, stmts[0]["Resource"])
    _, hasCond := stmts[0]["Condition"]
    assert.False(t, hasCond, "Condition should be absent when no hardening flags set")
}

// TestBuildS3Policy_WithExtensions verifies each extension becomes a
// separate Resource wildcard entry.
func TestBuildS3Policy_WithExtensions(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:            "photos",
        KeyPrefix:         "uploads/",
        AllowedExtensions: []string{".jpg", ".png"},
    })
    require.NoError(t, err)

    stmts := policy["Statement"].([]map[string]any)
    resources := stmts[0]["Resource"].([]string)
    assert.Equal(t, []string{
        "arn:aws:s3:::photos/uploads/*.jpg",
        "arn:aws:s3:::photos/uploads/*.png",
    }, resources)
}

// TestBuildS3Policy_BadExtensionFormat verifies extensions missing the '.'
// prefix are rejected.
func TestBuildS3Policy_BadExtensionFormat(t *testing.T) {
    _, err := buildS3Policy(&types.STSPolicy{
        Bucket:            "photos",
        KeyPrefix:         "uploads/",
        AllowedExtensions: []string{"jpg"},
    })
    require.Error(t, err)
    assert.Contains(t, err.Error(), "must start with '.'")
}

// TestBuildS3Policy_CustomActions verifies AllowedActions override default.
func TestBuildS3Policy_CustomActions(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:         "photos",
        KeyPrefix:      "uploads/",
        AllowedActions: []string{"s3:PutObject", "s3:GetObject"},
    })
    require.NoError(t, err)
    stmts := policy["Statement"].([]map[string]any)
    assert.Equal(t, []string{"s3:PutObject", "s3:GetObject"}, stmts[0]["Action"])
}

// TestBuildS3Policy_KeyPrefixTrailingSlashStripped verifies prefix
// normalization (no double slash).
func TestBuildS3Policy_KeyPrefixTrailingSlashStripped(t *testing.T) {
    for _, prefix := range []string{"uploads/", "uploads"} {
        policy, err := buildS3Policy(&types.STSPolicy{
            Bucket:    "photos",
            KeyPrefix: prefix,
        })
        require.NoError(t, err)
        stmts := policy["Statement"].([]map[string]any)
        resources := stmts[0]["Resource"].([]string)
        assert.Equal(t, []string{"arn:aws:s3:::photos/uploads/*"}, resources,
            "prefix %q should normalize", prefix)
    }
}

// TestBuildS3Policy_EmptyOrSlashKeyPrefix verifies empty or "/" KeyPrefix
// produces a single-slash resource base (no double slash).
func TestBuildS3Policy_EmptyOrSlashKeyPrefix(t *testing.T) {
    for _, prefix := range []string{"", "/", "//"} {
        policy, err := buildS3Policy(&types.STSPolicy{
            Bucket:    "photos",
            KeyPrefix: prefix,
        })
        require.NoError(t, err)
        stmts := policy["Statement"].([]map[string]any)
        resources := stmts[0]["Resource"].([]string)
        assert.Equal(t, []string{"arn:aws:s3:::photos/*"}, resources,
            "prefix %q should normalize to bucket-only resource", prefix)
    }
}

// TestBuildS3Policy_EnforceHTTPS verifies the Bool Condition that blocks
// plaintext HTTP uploads (AWS condition key: aws:SecureTransport).
func TestBuildS3Policy_EnforceHTTPS(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:       "photos",
        KeyPrefix:    "uploads/",
        EnforceHTTPS: true,
    })
    require.NoError(t, err)

    stmts := policy["Statement"].([]map[string]any)
    require.Len(t, stmts, 1)
    cond, ok := stmts[0]["Condition"].(map[string]any)
    require.True(t, ok, "Condition must be present when EnforceHTTPS is set")
    assert.Equal(t, map[string]any{
        "Bool": map[string]string{"aws:SecureTransport": "true"},
    }, cond)
}

// TestBuildS3Policy_LockObjectACL verifies the StringEquals Condition that
// forces uploaded objects to "private" (AWS condition key: s3:x-amz-acl).
func TestBuildS3Policy_LockObjectACL(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:        "photos",
        KeyPrefix:     "uploads/",
        LockObjectACL: true,
    })
    require.NoError(t, err)

    stmts := policy["Statement"].([]map[string]any)
    require.Len(t, stmts, 1)
    cond, ok := stmts[0]["Condition"].(map[string]any)
    require.True(t, ok, "Condition must be present when LockObjectACL is set")
    assert.Equal(t, map[string]any{
        "StringEquals": map[string]string{"s3:x-amz-acl": "private"},
    }, cond)
}

// TestBuildS3Policy_AllConditions verifies the two Condition operators can
// coexist in the same statement without colliding.
func TestBuildS3Policy_AllConditions(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:        "photos",
        KeyPrefix:     "uploads/",
        EnforceHTTPS:  true,
        LockObjectACL: true,
    })
    require.NoError(t, err)

    stmts := policy["Statement"].([]map[string]any)
    cond := stmts[0]["Condition"].(map[string]any)
    assert.Contains(t, cond, "Bool")
    assert.Contains(t, cond, "StringEquals")
}

// TestBuildS3Policy_DenyPutObjectACL verifies that enabling DenyPutObjectACL
// appends a second Deny statement targeting s3:PutObjectAcl on the same
// Resource set as the Allow statement.
func TestBuildS3Policy_DenyPutObjectACL(t *testing.T) {
    policy, err := buildS3Policy(&types.STSPolicy{
        Bucket:            "photos",
        KeyPrefix:         "uploads/",
        AllowedExtensions: []string{".jpg", ".png"},
        DenyPutObjectACL:  true,
    })
    require.NoError(t, err)

    stmts := policy["Statement"].([]map[string]any)
    require.Len(t, stmts, 2, "Allow + Deny statements expected")

    assert.Equal(t, "Allow", stmts[0]["Effect"])
    denyStmt := stmts[1]
    assert.Equal(t, "Deny", denyStmt["Effect"])
    assert.Equal(t, []string{"s3:PutObjectAcl"}, denyStmt["Action"])

    allowRes := stmts[0]["Resource"].([]string)
    denyRes := denyStmt["Resource"].([]string)
    assert.Equal(t, allowRes, denyRes, "Deny Resource must match Allow Resource")
}

// TestMarshalPolicyJSON_NoHTMLEscape verifies policy JSON marshals with HTML
// escaping disabled (same reason as Aliyun: some S3-compat backends reject
// &lt;/&gt;/&amp;).
func TestMarshalPolicyJSON_NoHTMLEscape(t *testing.T) {
    p := map[string]any{"k": "v<x>y&w"}
    out, err := marshalPolicyJSON(p)
    require.NoError(t, err)
    assert.Contains(t, string(out), "v<x>y&w")
    // JSON HTML-escaping turns `<` into the 6-char literal `<`. With
    // SetEscapeHTML(false) the output should NOT contain this escape form.
    // Note: in Go source the literal text `<` is written as `"\\u003c"`.
    assert.NotContains(t, string(out), "\\u003c")
    assert.False(t, strings.HasSuffix(string(out), "\n"), "trailing newline must be trimmed")
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOPROXY=https://goproxy.cn,direct go test -run 'TestBuildS3Policy|TestMarshalPolicyJSON' ./internal/provider/storage/s3/... 2>&1 | tail -10
```

Expected: build failure with `buildS3Policy undefined` / `marshalPolicyJSON undefined`.

- [ ] **Step 3: Implement `buildS3Policy` and `marshalPolicyJSON`**

Append to `internal/provider/storage/s3/sts.go` (after `newSTSClient`, before any future helpers):

```go
// --- internal helpers ---

// buildS3Policy translates STSPolicy into the JSON structure expected by
// AWS STS AssumeRole's Policy parameter. Returns map[string]any so the
// stsClient can marshal it with HTML escaping disabled.
//
// Translation rules:
//   - Bucket + KeyPrefix → Resource prefix "arn:aws:s3:::<bucket>/<prefix>/*"
//     (S3 ARNs have NO region/account segment — S3 is a global service.)
//   - AllowedExtensions (each must start with '.') → one Resource entry per ext
//   - AllowedActions defaults to ["s3:PutObject"] for credential hardening
//   - MaxSize is intentionally NOT mapped: S3 PutObject has no STS-side size
//     enforcement (same as Aliyun; only PostObject supports content-length-range).
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for s3:PutObjectAcl.
func buildS3Policy(p *types.STSPolicy) (map[string]any, error) {
    if p == nil {
        return nil, fmt.Errorf("nil sts policy")
    }
    if p.Bucket == "" {
        return nil, fmt.Errorf("sts policy: bucket is required")
    }

    actions := p.AllowedActions
    if len(actions) == 0 {
        actions = []string{"s3:PutObject"}
    }

    prefix := strings.Trim(p.KeyPrefix, "/")
    var base string
    if prefix == "" {
        base = fmt.Sprintf("arn:aws:s3:::%s/*", p.Bucket)
    } else {
        base = fmt.Sprintf("arn:aws:s3:::%s/%s/*", p.Bucket, prefix)
    }

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

    allowStmt := map[string]any{
        "Effect":   "Allow",
        "Action":   actions,
        "Resource": resources,
    }

    conditions := map[string]any{}
    if p.EnforceHTTPS {
        conditions["Bool"] = map[string]string{"aws:SecureTransport": "true"}
    }
    if p.LockObjectACL {
        conditions["StringEquals"] = map[string]string{"s3:x-amz-acl": "private"}
    }
    if len(conditions) > 0 {
        allowStmt["Condition"] = conditions
    }

    statements := []map[string]any{allowStmt}

    if p.DenyPutObjectACL {
        statements = append(statements, map[string]any{
            "Effect":   "Deny",
            "Action":   []string{"s3:PutObjectAcl"},
            "Resource": resources,
        })
    }

    return map[string]any{
        "Version":   "2012-10-17", // AWS IAM policy version
        "Statement": statements,
    }, nil
}

// marshalPolicyJSON marshals the policy map with HTML escaping disabled.
// Mirrors the Aliyun rationale: some S3-compatible backends reject HTML-
// escaped JSON. Trims the trailing newline added by json.Encoder.Encode.
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

Also update the imports at the top of `sts.go`:

```go
import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "strings"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/credentials"
    awssts "github.com/aws/aws-sdk-go-v2/service/sts"

    "storage-service/internal/provider/storage/types"
)
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOPROXY=https://goproxy.cn,direct go test -v -run 'TestBuildS3Policy|TestMarshalPolicyJSON' ./internal/provider/storage/s3/... 2>&1 | tail -20
```

Expected: 11 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/storage/s3/sts.go internal/provider/storage/s3/sts_test.go
git commit -m "feat(s3): add buildS3Policy translator + marshalPolicyJSON helper"
```

---

## Task 4: Implement `stsClient.assumeRole`

Calls AWS STS `AssumeRole` API. Tests use `httptest.NewServer` to mock the XML API.

**Files:**
- Modify: `internal/provider/storage/s3/sts.go`
- Modify: `internal/provider/storage/s3/sts_test.go`

- [ ] **Step 1: Write failing tests for `assumeRole`**

Append to `internal/provider/storage/s3/sts_test.go`:

```go
import (
    // add to existing import block:
    "net/http"
    "net/http/httptest"
)

// s3STSMockResp returns a minimal valid AWS STS AssumeRole XML response.
func s3STSMockResp() string {
    return `<?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>STS.ak123</AccessKeyId>
      <SecretAccessKey>STS.sk123</SecretAccessKey>
      <SessionToken>STS.token456</SessionToken>
      <Expiration>2026-06-23T15:30:00Z</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`
}

// TestS3AssumeRole_Success mocks the STS API and verifies the response mapping.
func TestS3AssumeRole_Success(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/xml")
        _, _ = w.Write([]byte(s3STSMockResp()))
    }))
    defer srv.Close()

    c, err := newSTSClient(&stsClientOpts{
        AccessKey: "ak",
        SecretKey: "sk",
        Region:    "us-east-1",
        Endpoint:  srv.URL,
    })
    require.NoError(t, err)

    duration := int64(900)
    resp, err := c.assumeRole(context.Background(), &assumeRoleReq{
        RoleArn:         "arn:aws:iam::123456789012:role/test",
        RoleSessionName: "owner-100",
        DurationSeconds: &duration,
        Policy: map[string]any{
            "Version": "2012-10-17",
            "Statement": []map[string]any{{
                "Effect":   "Allow",
                "Action":   []string{"s3:PutObject"},
                "Resource": []string{"arn:aws:s3:::bucket/uploads/*"},
            }},
        },
    })
    require.NoError(t, err)
    assert.Equal(t, "STS.ak123", resp.AccessKeyId)
    assert.Equal(t, "STS.sk123", resp.AccessKeySecret)
    assert.Equal(t, "STS.token456", resp.SecurityToken)
    assert.Equal(t, "2026-06-23T15:30:00Z", resp.Expiration)
}

// TestS3AssumeRole_APIError verifies SDK errors get wrapped with a clear prefix.
func TestS3AssumeRole_APIError(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(http.StatusForbidden)
        _, _ = w.Write([]byte(`<ErrorResponse><Error><Code>NoPermission</Code><Message>unauthorized</Message></Error></ErrorResponse>`))
    }))
    defer srv.Close()

    c, err := newSTSClient(&stsClientOpts{
        AccessKey: "ak",
        SecretKey: "sk",
        Region:    "us-east-1",
        Endpoint:  srv.URL,
    })
    require.NoError(t, err)

    duration := int64(900)
    _, err = c.assumeRole(context.Background(), &assumeRoleReq{
        RoleArn:         "arn:aws:iam::123456789012:role/test",
        RoleSessionName: "owner-100",
        DurationSeconds: &duration,
    })
    require.Error(t, err)
    assert.Contains(t, err.Error(), "assume role")
}

// TestS3AssumeRole_NilReq verifies nil req fails fast.
func TestS3AssumeRole_NilReq(t *testing.T) {
    c, err := newSTSClient(&stsClientOpts{
        AccessKey: "ak", SecretKey: "sk", Region: "us-east-1",
    })
    require.NoError(t, err)

    _, err = c.assumeRole(context.Background(), nil)
    require.Error(t, err)
    assert.Contains(t, err.Error(), "nil assume role req")
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOPROXY=https://goproxy.cn,direct go test -run 'TestS3AssumeRole' ./internal/provider/storage/s3/... 2>&1 | tail -5
```

Expected: `c.assumeRole undefined` build error.

- [ ] **Step 3: Implement `assumeRole` method**

In `internal/provider/storage/s3/sts.go`, add after `newSTSClient` (before `// --- internal helpers ---`):

```go
// assumeRole calls STS AssumeRole and maps the response to project types.
// A nil Policy is omitted so the role's full permissions apply.
func (c *stsClient) assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error) {
    if req == nil {
        return nil, fmt.Errorf("nil assume role req")
    }
    in := &awssts.AssumeRoleInput{
        RoleArn:         aws.String(req.RoleArn),
        RoleSessionName: aws.String(req.RoleSessionName),
        DurationSeconds: req.DurationSeconds,
    }
    if req.Policy != nil {
        policyBytes, err := marshalPolicyJSON(req.Policy)
        if err != nil {
            return nil, fmt.Errorf("marshal policy: %w", err)
        }
        in.Policy = aws.String(string(policyBytes))
    }

    out, err := c.cli.AssumeRole(ctx, in)
    if err != nil {
        return nil, fmt.Errorf("assume role: %w", err)
    }
    if out == nil || out.Credentials == nil {
        return nil, fmt.Errorf("assume role returned empty credentials")
    }
    return &assumeRoleResp{
        AccessKeyId:     aws.ToString(out.Credentials.AccessKeyId),
        AccessKeySecret: aws.ToString(out.Credentials.SecretAccessKey),
        SecurityToken:   aws.ToString(out.Credentials.SessionToken),
        Expiration:      out.Credentials.Expiration.Format("2006-01-02T15:04:05Z"),
    }, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
GOPROXY=https://goproxy.cn,direct go test -v -run 'TestS3AssumeRole' ./internal/provider/storage/s3/... 2>&1 | tail -10
```

Expected: 3 PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/storage/s3/sts.go internal/provider/storage/s3/sts_test.go
git commit -m "feat(s3): implement stsClient.assumeRole + XML API mock tests"
```

---

## Task 5: Implement `S3Provider.GetSTSToken` + remove stub

Replaces the stub in `provider.go` with the real implementation in `sts.go`.

**Files:**
- Modify: `internal/provider/storage/s3/sts.go` (add `GetSTSToken`)
- Modify: `internal/provider/storage/s3/provider.go` (delete stub)
- Modify: `internal/provider/storage/s3/sts_test.go` (add provider-level tests)

- [ ] **Step 1: Write failing tests for `GetSTSToken`**

Append to `internal/provider/storage/s3/sts_test.go`:

```go
import (
    // add to existing import block:
    "time"
)

// fakeS3STS is a minimal assumeRoleCaller stand-in for testing GetSTSToken
// without spinning up an HTTP server.
type fakeS3STS struct {
    gotReq *assumeRoleReq
    resp   *assumeRoleResp
    err    error
}

func (f *fakeS3STS) assumeRole(_ context.Context, req *assumeRoleReq) (*assumeRoleResp, error) {
    f.gotReq = req
    if f.err != nil {
        return nil, f.err
    }
    return f.resp, nil
}

// newS3ProviderWithFakeSTS bypasses the real constructor and wires the fake
// manually. If fake is nil the provider's stsCli field stays a nil interface
// so GetSTSToken's nil-guard fires correctly (Go interface-nil gotcha: a
// (*fakeS3STS, nil) boxed interface is non-nil).
func newS3ProviderWithFakeSTS(fake assumeRoleCaller, roleARN string) *S3Provider {
    p := &S3Provider{
        endpoint: "https://s3.example.com",
        region:   "us-east-1",
        roleARN:  roleARN,
    }
    if fake != nil {
        p.stsCli = fake
    }
    return p
}

// TestS3Provider_GetSTSToken_NoRoleARN verifies that a provider without
// RoleARN returns an explicit error rather than panicking on nil stsCli.
func TestS3Provider_GetSTSToken_NoRoleARN(t *testing.T) {
    p := newS3ProviderWithFakeSTS(nil, "")
    _, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
        Bucket:    "b",
        KeyPrefix: "p/",
        TTL:       15 * time.Minute,
    })
    require.Error(t, err)
    assert.Contains(t, err.Error(), "not configured")
}

// TestS3Provider_GetSTSToken_BelowMinTTL verifies a TTL below AWS STS 900s
// minimum is rejected locally with an actionable error.
func TestS3Provider_GetSTSToken_BelowMinTTL(t *testing.T) {
    fake := &fakeS3STS{resp: &assumeRoleResp{Expiration: "2026-06-23T15:30:00Z"}}
    p := newS3ProviderWithFakeSTS(fake, "arn:aws:iam::1:role/r")
    _, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
        Bucket:    "b",
        KeyPrefix: "p/",
        TTL:       5 * time.Minute,
    })
    require.Error(t, err)
    assert.Contains(t, err.Error(), "AWS STS minimum")
}

// TestS3Provider_GetSTSToken_Success verifies happy path.
func TestS3Provider_GetSTSToken_Success(t *testing.T) {
    fake := &fakeS3STS{
        resp: &assumeRoleResp{
            AccessKeyId:     "STS.ak",
            AccessKeySecret: "STS.sk",
            SecurityToken:   "STS.token",
            Expiration:      "2026-06-23T15:30:00Z",
        },
    }
    p := newS3ProviderWithFakeSTS(fake, "arn:aws:iam::1234:role/uploader")

    cred, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
        OwnerID:           100,
        OwnerType:         1,
        Bucket:            "photos",
        KeyPrefix:         "uploads/",
        AllowedExtensions: []string{".jpg", ".png"},
        TTL:               15 * time.Minute,
    })
    require.NoError(t, err)

    assert.Equal(t, "owner-100", fake.gotReq.RoleSessionName)
    assert.Equal(t, "arn:aws:iam::1234:role/uploader", fake.gotReq.RoleArn)
    require.NotNil(t, fake.gotReq.DurationSeconds)
    assert.Equal(t, int64(900), *fake.gotReq.DurationSeconds)

    assert.Equal(t, "STS.ak", cred.AccessKey)
    assert.Equal(t, "STS.sk", cred.SecretKey)
    assert.Equal(t, "STS.token", cred.SecurityToken)
    assert.Equal(t, "https://s3.example.com", cred.Endpoint)
    assert.Equal(t, "photos", cred.Bucket)
    assert.Equal(t, "uploads/", cred.ObjectKeyPrefix)
    expectedExpiry := time.Date(2026, 6, 23, 15, 30, 0, 0, time.UTC)
    assert.WithinDuration(t, expectedExpiry, cred.ExpiresAt, time.Second)
}

// TestS3Provider_GetSTSToken_BadExpiration verifies parse failure surfaces.
func TestS3Provider_GetSTSToken_BadExpiration(t *testing.T) {
    fake := &fakeS3STS{
        resp: &assumeRoleResp{Expiration: "not-a-date"},
    }
    p := newS3ProviderWithFakeSTS(fake, "arn:aws:iam::1:role/r")
    _, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
        Bucket:    "b",
        KeyPrefix: "p/",
        TTL:       15 * time.Minute,
    })
    require.Error(t, err)
    assert.Contains(t, err.Error(), "parse sts expiration")
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
GOPROXY=https://goproxy.cn,direct go test -run 'TestS3Provider_GetSTSToken' ./internal/provider/storage/s3/... 2>&1 | tail -10
```

Expected: build failure (depends on stub GetSTSToken still returning error immediately; the new tests want different behavior).

- [ ] **Step 3: Implement `GetSTSToken`**

In `internal/provider/storage/s3/sts.go`, add after the `stsClient.assumeRole` method (before `// --- internal helpers ---`):

```go
// GetSTSToken retrieves temporary STS credentials via AssumeRole. Requires
// RoleARN to be configured at NewS3Provider time; otherwise returns an
// explicit error so callers know to use GenerateUploadURL instead.
//
// Endpoint policy: if NewS3Provider was constructed with a custom endpoint
// (MinIO/S3-compat), STS hits that same endpoint; otherwise AWS regional STS.
func (p *S3Provider) GetSTSToken(ctx context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
    if p == nil || p.stsCli == nil || p.roleARN == "" {
        return nil, fmt.Errorf("s3 STS not configured for this provider; set provider.role_arn in config")
    }
    if policy == nil {
        return nil, fmt.Errorf("nil sts policy")
    }

    policyJSON, err := buildS3Policy(policy)
    if err != nil {
        return nil, fmt.Errorf("build sts policy: %w", err)
    }

    duration := int64(policy.TTL.Seconds())
    if duration <= 0 {
        return nil, fmt.Errorf("sts policy: TTL must be > 0")
    }
    if duration < minAWSSTSDuration {
        return nil, fmt.Errorf("sts policy: TTL %v below AWS STS minimum of %ds",
            policy.TTL, minAWSSTSDuration)
    }

    // RoleSessionName embeds OwnerID so S3 audit logs can trace credentials
    // back to the originating user.
    resp, err := p.stsCli.assumeRole(ctx, &assumeRoleReq{
        RoleArn:         p.roleARN,
        RoleSessionName: fmt.Sprintf("owner-%d", policy.OwnerID),
        DurationSeconds: &duration,
        Policy:          policyJSON,
    })
    if err != nil {
        return nil, fmt.Errorf("s3 sts assume role: %w", err)
    }

    expiresAt, err := time.Parse(time.RFC3339, resp.Expiration)
    if err != nil {
        return nil, fmt.Errorf("parse sts expiration %q: %w", resp.Expiration, err)
    }

    return &types.STSCredential{
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

Also add `"time"` to the import block in `sts.go`.

- [ ] **Step 4: Delete the stub `GetSTSToken` from `provider.go`**

In `internal/provider/storage/s3/provider.go`, remove:

```go
// GetSTSToken is not supported by the S3 provider.
// It returns an error indicating that STS is not available for S3-compatible backends.
func (S3Provider) GetSTSToken(_ context.Context, _ *types.STSPolicy) (*types.STSCredential, error) {
    return nil, fmt.Errorf("STS not supported by S3 provider")
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
GOPROXY=https://goproxy.cn,direct go test -v -run 'TestS3Provider_GetSTSToken' ./internal/provider/storage/s3/... 2>&1 | tail -10
```

Expected: 4 PASS.

- [ ] **Step 6: Run all s3 package tests**

```bash
GOPROXY=https://goproxy.cn,direct go test ./internal/provider/storage/s3/... 2>&1 | tail -5
```

Expected: `ok` (the pre-existing testcontainer S3 integration tests still fail on environment, that's OK).

- [ ] **Step 7: Commit**

```bash
git add internal/provider/storage/s3/sts.go internal/provider/storage/s3/sts_test.go internal/provider/storage/s3/provider.go
git commit -m "feat(s3): implement S3Provider.GetSTSToken via AWS STS AssumeRole"
```

---

## Task 6: Service-layer vendor-aware Action prefix

`issueUploadCredential` currently hardcodes `AllowedActions: ["oss:PutObject"]`. For S3 vendors this needs to be `["s3:PutObject"]`.

**Files:**
- Modify: `internal/service/upload/upload.go:651`
- Modify: `internal/service/upload/upload_test.go` (or `service_test.go` if vendor assertions live there)

- [ ] **Step 1: Locate where vendor is available in `issueUploadCredential`**

Run:

```bash
grep -n 'prepared.vendor\|VendorForBucket' internal/service/upload/upload.go | head -5
```

Expected output includes `prepared.vendor int32` (set by `prepareUpload` from `s.registry.VendorForBucket(bucket)`).

- [ ] **Step 2: Add vendor-aware Action prefix helper**

In `internal/provider/storage/types/types.go`, append a helper:

```go
// PutObjectActionForVendor returns the correct `PutObject` action string for
// the given vendor enum value. Used by service-layer code building STS
// policies that target a specific provider (aliyun vs s3 use different
// action prefixes).
//
// vendor is the int32 value of the proto Vendor enum (VENDOR_ALIYUN_OSS,
// VENDOR_AWS_S3, VENDOR_S3_COMPATIBLE, etc.). Unknown vendors default to
// the S3 prefix — fails open for forward-compat with new S3-compatible
// vendors added later.
func PutObjectActionForVendor(vendor int32) string {
    // Aliyun uses `oss:` prefix; everything else (AWS, MinIO, Ceph, etc.)
    // uses `s3:`. The proto enum values are defined in
    // gen/storage/v1/storage.pb.go.
    const vendorAliyunOSS int32 = 1 // VENDOR_ALIYUN_OSS — adjust if proto enum differs
    if vendor == vendorAliyunOSS {
        return "oss:PutObject"
    }
    return "s3:PutObject"
}
```

**Verify the proto enum value first** before committing:

```bash
grep -A 1 'VENDOR_ALIYUN_OSS\b' gen/storage/v1/storage.pb.go | head -5
```

If the value isn't `1`, fix the constant.

- [ ] **Step 3: Use the helper in `issueUploadCredential`**

In `internal/service/upload/upload.go:645-654`, replace the hardcoded Action with the vendor-aware helper. The file already imports `storage-service/internal/provider/storage/types` (from earlier ConfirmUpload work), so use that path:

```go
stsPolicy := &storage.STSPolicy{
    OwnerID:           ownerID,
    OwnerType:         ownerType,
    Bucket:            bucket,
    KeyPrefix:         prepared.bucketCfg.KeyPrefix,
    AllowedExtensions: allowedExtensions,
    AllowedActions:    []string{types.PutObjectActionForVendor(prepared.vendor)},
    MaxSize:           file.size,
    TTL:               prepared.resolvedTTL,
}
```

- [ ] **Step 4: Write a unit test for `PutObjectActionForVendor`**

Append to `internal/provider/storage/types/types_test.go` (create if missing):

```go
package types

import "testing"

func TestPutObjectActionForVendor(t *testing.T) {
    tests := []struct {
        name   string
        vendor int32
        want   string
    }{
        {"aliyun", 1, "oss:PutObject"},
        {"aws s3", 2, "s3:PutObject"},
        {"s3 compatible", 3, "s3:PutObject"},
        {"unknown", 99, "s3:PutObject"}, // forward-compat default
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            if got := PutObjectActionForVendor(tt.vendor); got != tt.want {
                t.Errorf("PutObjectActionForVendor(%d) = %q, want %q", tt.vendor, got, tt.want)
            }
        })
    }
}
```

Adjust the `1/2/3` values to match the actual proto enum.

- [ ] **Step 5: Run new test**

```bash
GOPROXY=https://goproxy.cn,direct go test -v -run 'TestPutObjectActionForVendor' ./internal/provider/storage/types/... 2>&1 | tail -10
```

Expected: PASS.

- [ ] **Step 6: Verify existing STS-related tests still pass**

```bash
GOPROXY=https://goproxy.cn,direct go test -run 'TestGetSTSCredential|TestBatchGetSTSCredential' ./internal/service/upload/... ./internal/service/ 2>&1 | tail -5
```

Expected: existing aliyun STS tests still pass (the vendor in tests is aliyun, so the prefix is still `oss:PutObject`).

- [ ] **Step 7: Commit**

```bash
git add internal/provider/storage/types/types.go internal/provider/storage/types/types_test.go internal/service/upload/upload.go
git commit -m "feat(upload): vendor-aware STS Action prefix (oss: vs s3:)"
```

---

## Task 7: Update `RoleARN` config comment + final verification

Pure doc update + end-to-end verification.

**Files:**
- Modify: `pkg/config/config.go:183-187`

- [ ] **Step 1: Update `RoleARN` comment**

In `pkg/config/config.go`:

```go
// RoleARN is the IAM/RAM role ARN to assume when minting STS credentials
// via GetSTSCredential. Format varies by vendor:
//   - Aliyun OSS: acs:ram::<account-id>:role/<role-name>
//   - AWS S3:     arn:aws:iam::<account-id>:role/<role-name>
//   - MinIO:      any non-empty identifier (MinIO doesn't validate ARN format)
// Empty = STS unavailable for this provider; clients must use
// GenerateUploadURL instead.
RoleARN string
```

Also update the related test comment at `pkg/config/config_test.go:317-322` if it specifically ties RoleARN to Aliyun.

- [ ] **Step 2: Run full lint + tests on touched packages**

```bash
GOPROXY=https://goproxy.cn,direct golangci-lint run ./internal/provider/storage/... ./internal/service/upload/... ./pkg/config/... 2>&1 | tail -10
GOPROXY=https://goproxy.cn,direct go test ./internal/provider/storage/s3/... ./internal/provider/storage/types/... ./internal/service/upload/... 2>&1 | tail -10
```

Expected: no NEW lint issues (pre-existing `AliyunProcessor`/`AliyunProvider`/`S3Provider` stutter warnings are out of scope); all touched package tests pass except pre-existing S3 testcontainer env failures.

- [ ] **Step 3: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "docs(config): generalize RoleARN comment for multi-vendor STS"
```

---

## Verification (run after all tasks)

- [ ] `GOPROXY=https://goproxy.cn,direct go build ./...` passes
- [ ] `GOPROXY=https://goproxy.cn,direct go test -race ./internal/provider/storage/... ./internal/service/upload/... ./pkg/xcodes/` passes (excluding pre-existing S3 testcontainer env failures)
- [ ] `GOPROXY=https://goproxy.cn,direct golangci-lint run ./internal/provider/storage/s3/...` shows no NEW issues vs baseline
- [ ] `make build` produces both `bin/server` and `bin/migrate`
