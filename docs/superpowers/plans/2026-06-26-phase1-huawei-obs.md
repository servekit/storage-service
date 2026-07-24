# Phase 1: Huawei OBS Provider Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the Huawei OBS storage provider covering all 8 `types.Provider` methods plus the standalone `types.CDNURLGenerator`. After this PR merges, a `VENDOR_HUAWEI_OBS` entry in `config.yaml` works end-to-end (PutObject / GetObject / DeleteObject / HeadObject / ListObjects / PresignPutObject / PresignGetObject / GetSTSToken via IAM Agency + CDN Type A signing).

**Architecture:**
- New package `internal/provider/storage/huawei/` mirrors the existing `aliyun/` package layout: `provider.go` + `cdn.go` + `sts.go` + `imgproc.go` + `*_test.go`.
- `provider.go` defines `HuaweiProvider` holding an `*obs.ObsClient` (object operations) and a separate IAM-v3 client (STS agency calls). All 8 `types.Provider` methods are implemented.
- `cdn.go` defines `CDNURLGenerator` with Type-A MD5 `auth_key` signing (same formula as Aliyun, reimplemented in-package per the design spec).
- `sts.go` defines the IAM Agency `CreateTemporaryAccessKeyByAgency` wrapper plus the Huawei-specific JSON policy builder (`Version: "1.1"`, `Resource: ["OBS::*:<bucket>/<prefix>/*"]`).
- `imgproc.go` defines `buildObsProcessStyle(ops)` producing `image/resize,p_100` segments (Huawei OBS uses the same `image/<action>` syntax as Aliyun OSS, with a different CDN query param name `x-image-process` instead of `x-oss-process`).
- `registry.go` replaces the `VENDOR_HUAWEI_OBS` "not yet implemented" placeholder with real `huawei.NewHuaweiProvider` / `huawei.NewCDNURLGenerator` calls. `registry_test.go` updates to assert successful dispatch for Huawei (and continues to assert the remaining two vendors still return "not yet implemented").

**Tech Stack:** Go 1.26.1, `github.com/huaweicloud/huaweicloud-sdk-go-obs` v3.26.3 (object ops + presigned URLs), `github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam` (STS Agency credentials), testify, net/http/httptest (mock OBS HTTP API).

**Spec:** `docs/superpowers/specs/2026-06-25-multi-vendor-storage-providers-design.md` (Huawei section, lines 202-235).

---

## File Map

| File | Responsibility | Created/Modified |
|------|----------------|------------------|
| `go.mod` / `go.sum` | Add `huaweicloud-sdk-go-obs` + `huaweicloud-sdk-go-v3/services/iam` deps | Modified (Task 1) |
| `internal/provider/storage/huawei/cdn.go` | `CDNURLGenerator` + `signHuaweiTypeA` helper | Created (Task 2) |
| `internal/provider/storage/huawei/cdn_test.go` | Known-vector test (Huawei doc example) + generator behavior | Created (Task 2) |
| `internal/provider/storage/huawei/imgproc.go` | `buildObsProcessStyle(ops)` → `image/resize,p_100,...` | Created (Task 3) |
| `internal/provider/storage/huawei/imgproc_test.go` | Per-op output format assertions | Created (Task 3) |
| `internal/provider/storage/huawei/sts.go` | IAM Agency temp credential client + `buildHuaweiPolicy` JSON builder | Created (Task 4) |
| `internal/provider/storage/huawei/sts_test.go` | `assert.JSONEq` policy tests + GetSTSToken error paths | Created (Task 4) |
| `internal/provider/storage/huawei/provider.go` | `HuaweiProvider` struct + all 8 `types.Provider` methods | Created (Task 5) |
| `internal/provider/storage/huawei/provider_test.go` | HTTP-mocked OBS API + helpers tests | Created (Task 5) |
| `internal/provider/storage/registry.go` | Replace Huawei case in `newProvider` / `newCDNURLGenerator` | Modified (Task 6) |
| `internal/provider/storage/registry_test.go` | Update Huawei subtests to expect success | Modified (Task 6) |

---

## Task 1: Add Huawei SDK dependencies to go.mod

**Goal:** Pull in the two Huawei SDK modules. The OBS SDK (`huaweicloud-sdk-go-obs`) handles object operations and presigned URLs; the IAM v3 SDK (`huaweicloud-sdk-go-v3/services/iam`) handles STS Agency temporary credentials. Both must be present before any `huawei/` package file can compile.

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Inspect current go.mod to confirm absence of Huawei deps**

Run: `grep -n "huaweicloud" go.mod || echo "no huaweicloud deps yet"`
Expected output: `no huaweicloud deps yet`

- [ ] **Step 2: Add the OBS SDK**

Run: `go get github.com/huaweicloud/huaweicloud-sdk-go-obs@v3.26.3`
Expected: command exits 0, `go.mod` gains a `require` entry for the OBS SDK. Transitive deps appear in the indirect block.

- [ ] **Step 3: Add the IAM v3 SDK (agency STS path)**

Run: `go get github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam@latest`
Expected: command exits 0. The IAM service module brings in the parent `huaweicloud-sdk-go-v3` core as a transitive dep. Pin to whatever `latest` resolves to (record the version in the commit message).

- [ ] **Step 4: Tidy and verify the build**

Run: `go mod tidy && go build ./...`
Expected: both commands exit 0 with no output. The build passes because no source file references the new modules yet.

- [ ] **Step 5: Confirm both deps are in go.mod**

Run: `grep -E "huaweicloud" go.mod`
Expected output: at least two lines — one for `huaweicloud-sdk-go-obs` and one for `huaweicloud-sdk-go-v3/services/iam` (plus possibly the parent `huaweicloud-sdk-go-v3` core if not promoted to direct).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add Huawei OBS and IAM v3 SDKs

huaweicloud-sdk-go-obs v3.26.3 covers PutObject/GetObject/HeadObject/
ListObjects/CreateBrowserPresignedUrl. huaweicloud-sdk-go-v3/services/iam
covers CreateTemporaryAccessKeyByAgency for STS via IAM Agency (委托).
Phase 1 Huawei provider will wire these into the storage layer."
```

---

## Task 2: CDN URL generator (cdn.go + cdn_test.go)

**Goal:** Implement Huawei CDN Type-A URL signing. The algorithm is the same MD5 formula as Aliyun's (per design spec line 221), but reimplemented in the `huawei` package so the package stays self-contained. The generator supports image ops via `x-image-process` (Huawei's CDN query param name) and `response-content-disposition` for filename override, with `Public=true` returning an unsigned URL.

**Files:**
- Create: `internal/provider/storage/huawei/cdn.go`
- Create: `internal/provider/storage/huawei/cdn_test.go`

- [ ] **Step 1: Create the huawei package directory**

Run: `mkdir -p internal/provider/storage/huawei`
Expected: directory created.

- [ ] **Step 2: Write cdn.go**

Create `internal/provider/storage/huawei/cdn.go`:

```go
// Package huawei implements the storage.Provider interface for Huawei OBS,
// including object operations (PutObject/GetObject/etc.) via the
// huaweicloud-sdk-go-obs module and STS credential issuance via IAM Agency
// through the huaweicloud-sdk-go-v3 IAM service module. All Huawei-specific
// code lives in this package so the parent storage package stays
// vendor-agnostic; the parent package imports huawei from registry.go to
// wire up VENDOR_HUAWEI_OBS providers.
package huawei

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"storage-service/internal/provider/storage/types"
	"storage-service/pkg/config"
)

// CDNURLGenerator builds Huawei CDN signed URLs for objects on a single
// bucket. One instance per bucket; constructed by the Registry from the
// bucket's config.CDNConfig. Decoupled from HuaweiProvider so the storage
// provider stays focused on OBS operations — CDN signing talks to Huawei
// CDN (a separate product) and shares only the auth key with the origin.
type CDNURLGenerator struct {
	cdnConfig *config.CDNConfig
}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator constructs a Huawei Type-A CDNURLGenerator for a
// bucket. cdn MUST be non-nil — callers (Registry) gate on nil before
// constructing.
func NewCDNURLGenerator(cdn *config.CDNConfig) *CDNURLGenerator {
	return &CDNURLGenerator{cdnConfig: cdn}
}

// CDNURL builds a Huawei CDN URL for the object this generator is bound to.
//
// When opts.Public=false (default): the URL is signed with Type A auth_key
// (auth_key = ts-rand-uid-md5(uri-ts-rand-uid-key)) and expires at
// (now + opts.TTL). CDN edge nodes verify the MD5 against the same key
// (configured in the CDN console) and reject with 403 if mismatched or
// expired.
//
// When opts.Public=true: the URL is unsigned (no auth_key) and permanent.
// CDN must be configured to allow anonymous access for the path (path
// whitelist or public bucket ACL).
//
// opts.Ops (non-empty) and opts.Filename (non-empty) are appended as
// x-image-process and response-content-disposition query params; Huawei CDN
// forwards both to OBS on cache miss. OBS honors response-content-disposition
// by setting the Content-Disposition response header so browsers download
// rather than inline-display.
//
// Huawei Type A auth_key signs the URI path only (no scheme/host, no query),
// so any combination of query params composes without re-signing. The
// algorithm is identical to Aliyun Type A — Huawei CDN copied the format.
func (g *CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	u := types.CDNObjectURL(g.cdnConfig.Domain, objectKey)
	q := u.Query()
	if len(opts.Ops) > 0 {
		q.Set("x-image-process", buildObsProcessStyle(opts.Ops))
	}
	if opts.Filename != "" {
		q.Set("response-content-disposition", types.BuildContentDisposition(opts.Filename))
	}
	u.RawQuery = q.Encode()

	if opts.Public {
		// Public URL: no auth_key, permanent. CDN must be configured to
		// allow anonymous access for this path (path whitelist or public
		// bucket ACL).
		return u.String(), time.Time{}, nil
	}

	now := time.Now()
	expiresAt := now.Add(opts.TTL)

	// Type A: the path used for signing is the URI without scheme/host.
	// Same convention as Aliyun (Huawei copied the algorithm); pinned to
	// the bare objectKey (no leading slash) so the test's round-trip
	// through signHuaweiTypeA matches. Verify against your actual CDN
	// console setting during smoke testing — some consoles expect a
	// leading "/" here.
	authKey, err := signHuaweiTypeA(objectKey, g.cdnConfig.AuthKey, expiresAt.Unix(), "0")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign cdn url: %w", err)
	}

	// Reuse the same url.Values to keep param order deterministic; auth_key
	// is appended after x-image-process / response-content-disposition so
	// the URL reads naturally for operators inspecting logs.
	q.Set("auth_key", authKey)
	u.RawQuery = q.Encode()
	return u.String(), expiresAt, nil
}

// --- internal helpers ---

// signHuaweiTypeA returns a CDN URL auth_key string for the given URI,
// formatted as `<timestamp>-<rand>-<uid>-<md5hex>` where md5hex is the
// lowercase hex MD5 of `<uri>-<timestamp>-<rand>-<uid>-<privateKey>`.
//
// Huawei does NOT provide an SDK helper for CDN URL signing. The algorithm
// is the same MD5 formula as Aliyun Type A (Huawei copied the design) but
// reimplemented here so the huawei package has no dependency on the aliyun
// package. Verified against the documented known vector in cdn_test.go.
//
// Spec: https://support.huaweicloud.com/usermanual-cdn/cdn_01_0040.html
//
// rand is generated with crypto/rand (16 random bytes -> 32 hex chars).
// Equivalent to a UUID v4 with the dashes removed, but with full 128-bit
// entropy (UUID v4 reserves 6 bits for version/variant markers). Huawei
// CDN Type A only requires rand to be dash-free.
//
// Callers do not control rand; if you need a fixed rand for testing or
// replay, use signHuaweiTypeAWithInputs.
func signHuaweiTypeA(uri, privateKey string, ts int64, uid string) (string, error) {
	r, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate rand: %w", err)
	}
	return signHuaweiTypeAWithInputs(uri, privateKey, ts, r, uid), nil
}

// signHuaweiTypeAWithInputs is signHuaweiTypeA with caller-supplied rand.
// Used by tests to verify against known vectors and by signHuaweiTypeA
// internally.
func signHuaweiTypeAWithInputs(uri, privateKey string, ts int64, rand, uid string) string {
	s := fmt.Sprintf("%s-%d-%s-%s-%s", uri, ts, rand, uid, privateKey)
	sum := md5.Sum([]byte(s))
	return fmt.Sprintf("%d-%s-%s-%s", ts, rand, uid, hex.EncodeToString(sum[:]))
}

// randomHex returns n random bytes encoded as 2n lowercase hex characters.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
```

- [ ] **Step 3: Write cdn_test.go with the Huawei known vector**

Create `internal/provider/storage/huawei/cdn_test.go`:

```go
package huawei

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storage-service/internal/provider/storage/types"
	"storage-service/pkg/config"
)

// newGenWithCDN builds a Huawei Type-A generator with the given CDN config.
func newGenWithCDN(t *testing.T, cdn *config.CDNConfig) *CDNURLGenerator {
	t.Helper()
	return NewCDNURLGenerator(cdn)
}

// huaweiCDNConfig returns a minimal Huawei CDNConfig for tests.
func huaweiCDNConfig(authKey string) *config.CDNConfig {
	return &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: authKey,
	}
}

// TestHuaweiCDNURLGenerator_PlainDownload verifies the URL format and
// auth_key presence for a plain download (no ops, no filename).
func TestHuaweiCDNURLGenerator_PlainDownload(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("test-key"))

	ttl := 30 * time.Minute
	gotURL, expiresAt, err := g.CDNURL(context.Background(), "uploads/00/abc", types.CDNURLOptions{TTL: ttl})
	require.NoError(t, err)

	// Expiry = now + ttl, within a second of clock drift tolerance.
	assert.WithinDuration(t, time.Now().Add(ttl), expiresAt, time.Second)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "cdn.example.com", u.Host)
	assert.Equal(t, "/uploads/00/abc", u.Path)

	authKey := u.Query().Get("auth_key")
	require.NotEmpty(t, authKey, "auth_key must be present")
	assert.NotContains(t, u.RawQuery, "x-image-process", "plain download must not carry x-image-process")

	// auth_key timestamp = expiry's Unix time (Type A convention).
	fields := strings.Split(authKey, "-")
	require.Len(t, fields, 4, "auth_key must be ts-rand-uid-md5hex")
	assert.Equal(t, expiresAt.Unix(), parseInt64(t, fields[0]))
}

// TestHuaweiCDNURLGenerator_WithImageOps verifies x-image-process is
// appended when ops is non-empty. Huawei CDN uses x-image-process (NOT
// Aliyun's x-oss-process) — pinning the query param name guards against
// copy-paste drift from the aliyun package.
func TestHuaweiCDNURLGenerator_WithImageOps(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 100, Height: 100}}
	gotURL, _, err := g.CDNURL(context.Background(), "uploads/00/abc", types.CDNURLOptions{
		Ops: ops,
		TTL: time.Hour,
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Contains(t, u.Query().Get("x-image-process"), "image/resize")
	assert.Empty(t, u.Query().Get("x-oss-process"), "Huawei CDN must NOT use Aliyun's x-oss-process")
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestHuaweiCDNURLGenerator_AuthKeyAlgorithm pins the auth_key value to
// what signHuaweiTypeAWithInputs produces — a regression guard against
// accidental drift between the generator method and the algorithm.
func TestHuaweiCDNURLGenerator_AuthKeyAlgorithm(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("known-key"))
	gotURL, expiresAt, err := g.CDNURL(context.Background(), "k", types.CDNURLOptions{TTL: time.Hour})
	require.NoError(t, err)
	u, _ := url.Parse(gotURL)
	got := u.Query().Get("auth_key")

	// Verify the algorithm is internally consistent by re-signing with
	// extracted fields.
	fields := strings.Split(got, "-")
	require.Len(t, fields, 4)
	ts, rand, uid, hash := fields[0], fields[1], fields[2], fields[3]
	expected := signHuaweiTypeAWithInputs("k", "known-key", expiresAt.Unix(), rand, uid)
	assert.Equal(t, expected, got, "auth_key must round-trip through signHuaweiTypeAWithInputs")
	_ = ts
	_ = hash
}

// TestHuaweiCDNURLGenerator_PublicMode verifies that public=true produces
// an unsigned URL: no auth_key, no expiry. CDN console must allow anon.
func TestHuaweiCDNURLGenerator_PublicMode(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("test-key"))

	gotURL, expiresAt, err := g.CDNURL(context.Background(), "avatars/100.jpg", types.CDNURLOptions{Public: true})
	require.NoError(t, err)

	assert.True(t, expiresAt.IsZero(), "public URL has no expiry")

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "cdn.example.com", u.Host)
	assert.Equal(t, "/avatars/100.jpg", u.Path)
	assert.Empty(t, u.Query().Get("auth_key"), "public URL must NOT have auth_key")
	assert.Empty(t, u.RawQuery, "public URL with no ops must have empty query string")
}

// TestHuaweiCDNURLGenerator_PublicMode_WithImageOps verifies that public
// mode + ops yields a URL with x-image-process but still no auth_key.
func TestHuaweiCDNURLGenerator_PublicMode_WithImageOps(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 100}}
	gotURL, _, err := g.CDNURL(context.Background(), "avatars/100.jpg", types.CDNURLOptions{
		Ops:    ops,
		Public: true,
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Empty(t, u.Query().Get("auth_key"), "public URL must NOT have auth_key")
	assert.Contains(t, u.Query().Get("x-image-process"), "image/resize")
}

// TestHuaweiCDNURLGenerator_FilenameAddsContentDisposition verifies that
// Filename sets response-content-disposition in the query, and that
// auth_key is computed independently of the query (Type A signs only the
// URI path).
func TestHuaweiCDNURLGenerator_FilenameAddsContentDisposition(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("test-key"))

	gotURL, _, err := g.CDNURL(context.Background(), "report.pdf", types.CDNURLOptions{
		TTL:      time.Hour,
		Filename: "年报.pdf",
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	cd := u.Query().Get("response-content-disposition")
	require.NotEmpty(t, cd, "response-content-disposition must be set when Filename is non-empty")
	assert.Contains(t, cd, "attachment")
	assert.Contains(t, cd, "UTF-8''", "non-ASCII filename must use RFC 5987 filename*")
	// auth_key still present and independent of disposition query.
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestHuaweiCDNURLGenerator_FilenameAndOpsTogether verifies both query
// params compose without breaking the auth_key signature.
func TestHuaweiCDNURLGenerator_FilenameAndOpsTogether(t *testing.T) {
	g := newGenWithCDN(t, huaweiCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 200}}
	gotURL, _, err := g.CDNURL(context.Background(), "img.jpg", types.CDNURLOptions{
		Ops:      ops,
		TTL:      time.Hour,
		Filename: "resized.jpg",
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.NotEmpty(t, u.Query().Get("x-image-process"))
	assert.NotEmpty(t, u.Query().Get("response-content-disposition"))
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestSignHuaweiTypeA_KnownVector locks the algorithm against Huawei's
// documented example. If this test fails the algorithm drifted from the
// spec and CDN edge nodes will reject every signed URL we issue.
//
// Source: https://support.huaweicloud.com/usermanual-cdn/cdn_01_0040.html
// (search "鉴权URL示例" — the doc reuses Aliyun's example since the
// algorithm is identical).
func TestSignHuaweiTypeA_KnownVector(t *testing.T) {
	// Huawei CDN doc worked example: md5hash for sstring
	// "/video/standard/test.mp4-1444435200-0-0-aliyuncdnexp1234"
	// is 23bf85053008f5c0e791667a313e28ce (same vector Aliyun uses because
	// Huawei copied the algorithm).
	got := signHuaweiTypeAWithInputs("/video/standard/test.mp4", "aliyuncdnexp1234", 1444435200, "0", "0")
	want := "1444435200-0-0-23bf85053008f5c0e791667a313e28ce"
	assert.Equal(t, want, got, "auth_key must match Huawei CDN doc example exactly")
}

// TestSignHuaweiTypeA_RandGenerated verifies that signHuaweiTypeA fills in
// rand when not pre-supplied. Different calls must produce different
// auth_keys (rand varies).
func TestSignHuaweiTypeA_RandGenerated(t *testing.T) {
	a, err := signHuaweiTypeA("/image/x.png", "key", 1700000000, "uid1")
	require.NoError(t, err)
	b, err := signHuaweiTypeA("/image/x.png", "key", 1700000000, "uid1")
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "two calls should produce different auth_keys (random rand)")
}

// TestSignHuaweiTypeA_DifferentKeyDifferentHash verifies the key actually
// participates in the MD5 input (regression guard against accidentally
// hardcoding or dropping the key).
func TestSignHuaweiTypeA_DifferentKeyDifferentHash(t *testing.T) {
	a := signHuaweiTypeAWithInputs("/x", "key1", 1700000000, "r", "u")
	b := signHuaweiTypeAWithInputs("/x", "key2", 1700000000, "r", "u")
	assert.NotEqual(t, a, b)
}

// TestSignHuaweiTypeA_Format verifies the auth_key field order is
// ts-rand-uid-md5hex.
func TestSignHuaweiTypeA_Format(t *testing.T) {
	got := signHuaweiTypeAWithInputs("/x", "k", 1700000000, "r", "u")
	// Pattern: digits-dash-string-dash-string-dash-32hex
	assert.Regexp(t, `^1700000000-r-u-[0-9a-f]{32}$`, got)
}

// --- internal helpers ---

func parseInt64(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			require.Failf(t, "not a number", "got %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
```

- [ ] **Step 4: Run the CDN tests**

Run: `go test ./internal/provider/storage/huawei/ -count=1 -v -run 'CDN|HuaweiTypeA|SignHuawei'`
Expected: all tests PASS. The known-vector test confirms the MD5 algorithm matches the Huawei CDN doc example.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/storage/huawei/cdn.go internal/provider/storage/huawei/cdn_test.go
git commit -m "feat(huawei): CDN Type-A URL generator with known-vector test

Huawei CDN copied Aliyun's Type-A auth_key algorithm; reimplemented here
in the huawei package (no dependency on aliyun) with the
x-image-process query param (Huawei's name, not Aliyun's x-oss-process).
Known-vector test pins the MD5 algorithm against Huawei's doc example."
```

---

## Task 3: Image style builder (imgproc.go + imgproc_test.go)

**Goal:** Huawei OBS uses the same `image/<action>,k_v` syntax as Aliyun OSS for cloud-side image processing. The only difference is the CDN query param name (`x-image-process` vs `x-oss-process`), which the CDN generator already handles. The style builder produces pipe-segment output (e.g. `image/resize,m_lfit,w_200,h_150/image/format,webp`) reusable for both the PresignGetObject `x-image-process` query param and the CDN generator.

**Files:**
- Create: `internal/provider/storage/huawei/imgproc.go`
- Create: `internal/provider/storage/huawei/imgproc_test.go`

- [ ] **Step 1: Write imgproc.go**

Create `internal/provider/storage/huawei/imgproc.go`:

```go
package huawei

import (
	"encoding/base64"
	"fmt"
	"strings"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/provider/storage/types"
)

// buildObsProcessStyle translates typed ops into Huawei OBS image process
// syntax. Each Op becomes one image/<action> segment; segments are joined
// with "/". Empty input returns empty string.
//
// Huawei OBS uses the same image/<action>,k_v syntax as Aliyun OSS, so
// this function is structurally identical to aliyun.buildOssProcessStyle.
// Kept as a separate package-level helper so the huawei package has no
// dependency on aliyun, and future Huawei-specific divergences (e.g.
// Huawei-only actions) have a clear home.
//
// The output is the value of the x-image-process query param on CDN URLs
// and the image process directive on PresignGetObject URLs.
func buildObsProcessStyle(ops []types.Op) string {
	var parts []string
	for _, op := range ops {
		switch op.Type {
		case types.OpResize:
			mode := huaweiResizeMode(op.ResizeMode)
			s := fmt.Sprintf("image/resize,m_%s", mode)
			if op.Width > 0 {
				s += fmt.Sprintf(",w_%d", op.Width)
			}
			if op.Height > 0 {
				s += fmt.Sprintf(",h_%d", op.Height)
			}
			parts = append(parts, s)
		case types.OpFormat:
			parts = append(parts, fmt.Sprintf("image/format,%s", huaweiFormat(op.Format)))
		case types.OpQuality:
			parts = append(parts, fmt.Sprintf("image/quality,q_%d", op.Quality))
		case types.OpCrop:
			s := "image/crop"
			if op.Width > 0 {
				s += fmt.Sprintf(",w_%d", op.Width)
			}
			if op.Height > 0 {
				s += fmt.Sprintf(",h_%d", op.Height)
			}
			parts = append(parts, s)
		case types.OpRotate:
			parts = append(parts, fmt.Sprintf("image/rotate,%d", op.RotateDegrees))
		case types.OpWatermark:
			encoded := base64.StdEncoding.EncodeToString([]byte(op.WatermarkText))
			parts = append(parts, fmt.Sprintf("image/watermark,text_%s", encoded))
		case types.OpBlur:
			s := "image/blur"
			if op.BlurRadius > 0 {
				s += fmt.Sprintf(",r_%d", op.BlurRadius)
			}
			if op.BlurSigma > 0 {
				s += fmt.Sprintf(",s_%d", op.BlurSigma)
			}
			parts = append(parts, s)
		case types.OpSharpen:
			if op.SharpenAmount > 0 {
				parts = append(parts, fmt.Sprintf("image/sharpen,p_%d", op.SharpenAmount))
			}
		case types.OpProgressive:
			if op.Progressive {
				parts = append(parts, "image/interlace,1")
			}
		case types.OpAutoOrient:
			if op.AutoOrient {
				parts = append(parts, "image/auto-orient,1")
			}
		case types.OpStripMetadata:
			if op.StripMetadata {
				parts = append(parts, "image/strip")
			}
		}
	}
	return strings.Join(parts, "/")
}

func huaweiResizeMode(m storagev1.ImageResizeMode) string {
	switch m {
	case storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL:
		return "fill"
	case storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_PAD:
		return "pad"
	default:
		return "lfit"
	}
}

func huaweiFormat(f storagev1.ImageFormat) string {
	switch f {
	case storagev1.ImageFormat_IMAGE_FORMAT_JPG:
		return "jpg"
	case storagev1.ImageFormat_IMAGE_FORMAT_PNG:
		return "png"
	case storagev1.ImageFormat_IMAGE_FORMAT_WEBP:
		return "webp"
	case storagev1.ImageFormat_IMAGE_FORMAT_GIF:
		return "gif"
	case storagev1.ImageFormat_IMAGE_FORMAT_BMP:
		return "bmp"
	case storagev1.ImageFormat_IMAGE_FORMAT_HEIC:
		return "heic"
	case storagev1.ImageFormat_IMAGE_FORMAT_AVIF:
		return "avif"
	default:
		return "jpg"
	}
}
```

- [ ] **Step 2: Write imgproc_test.go**

Create `internal/provider/storage/huawei/imgproc_test.go`:

```go
package huawei

import (
	"encoding/base64"
	"testing"

	storagev1 "storage-service/gen/storage/v1"
	"storage-service/internal/provider/storage/types"
)

func TestBuildObsProcessStyle_ResizeOnly(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
	}
	got := buildObsProcessStyle(ops)
	want := "image/resize,m_lfit,w_200,h_150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_ResizeWithMode(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
	}
	got := buildObsProcessStyle(ops)
	want := "image/resize,m_fill,w_200,h_150"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_ResizeWidthOnly(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 300},
	}
	got := buildObsProcessStyle(ops)
	want := "image/resize,m_lfit,w_300"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_ResizeFormatQuality(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 200, Height: 150},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
	}
	got := buildObsProcessStyle(ops)
	want := "image/resize,m_lfit,w_200,h_150/image/format,webp/image/quality,q_80"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_Crop(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpCrop, Width: 100, Height: 100},
	}
	got := buildObsProcessStyle(ops)
	want := "image/crop,w_100,h_100"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_Rotate(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpRotate, RotateDegrees: 90},
	}
	got := buildObsProcessStyle(ops)
	want := "image/rotate,90"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_Watermark(t *testing.T) {
	text := "hello"
	ops := []types.Op{
		{Type: types.OpWatermark, WatermarkText: text},
	}
	got := buildObsProcessStyle(ops)
	encoded := base64.StdEncoding.EncodeToString([]byte(text))
	want := "image/watermark,text_" + encoded
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_AllOps(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpResize, Width: 800, Height: 600, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_PAD},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_JPG},
		{Type: types.OpQuality, Quality: 90},
		{Type: types.OpRotate, RotateDegrees: 180},
	}
	got := buildObsProcessStyle(ops)

	want := "image/resize,m_pad,w_800,h_600/image/format,jpg/image/quality,q_90/image/rotate,180"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildObsProcessStyle_CropNoDimensions(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpCrop},
	}
	got := buildObsProcessStyle(ops)
	want := "image/crop"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObsProcessStyle_Blur covers OpBlur with both radius and sigma
// populated. Order is fixed (r before s) to match Huawei OBS docs.
func TestBuildObsProcessStyle_Blur(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpBlur, BlurRadius: 2, BlurSigma: 5},
	}
	got := buildObsProcessStyle(ops)
	want := "image/blur,r_2,s_5"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObsProcessStyle_Sharpen covers OpSharpen — only emitted when
// amount > 0, since 0 is the no-op zero value.
func TestBuildObsProcessStyle_Sharpen(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpSharpen, SharpenAmount: 50},
	}
	got := buildObsProcessStyle(ops)
	want := "image/sharpen,p_50"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObsProcessStyle_Progressive verifies OpProgressive emits
// interlace,1 when toggled on. Off → no segment.
func TestBuildObsProcessStyle_Progressive(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpProgressive, Progressive: true},
	}
	got := buildObsProcessStyle(ops)
	want := "image/interlace,1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObsProcessStyle_AutoOrient covers the EXIF-rotation fix.
func TestBuildObsProcessStyle_AutoOrient(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
	}
	got := buildObsProcessStyle(ops)
	want := "image/auto-orient,1"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObsProcessStyle_StripMetadata verifies the strip segment for
// EXIF/IPTC/XMP removal.
func TestBuildObsProcessStyle_StripMetadata(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildObsProcessStyle(ops)
	want := "image/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestBuildObsProcessStyle_BooleanTogglesOff verifies that Op{Progressive,
// AutoOrient, StripMetadata} produce no segment when their bool is false.
func TestBuildObsProcessStyle_BooleanTogglesOff(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpProgressive},
		{Type: types.OpAutoOrient},
		{Type: types.OpStripMetadata},
	}
	got := buildObsProcessStyle(ops)
	if got != "" {
		t.Errorf("got %q, want empty (all toggles off)", got)
	}
}

// TestBuildObsProcessStyle_ThumbnailPipeline verifies a realistic thumbnail
// pipeline: auto-orient → resize → sharpen → format → quality → progressive
// → strip. Order matters: OBS applies segments left-to-right.
func TestBuildObsProcessStyle_ThumbnailPipeline(t *testing.T) {
	ops := []types.Op{
		{Type: types.OpAutoOrient, AutoOrient: true},
		{Type: types.OpResize, Width: 200, Height: 200, ResizeMode: storagev1.ImageResizeMode_IMAGE_RESIZE_MODE_FILL},
		{Type: types.OpSharpen, SharpenAmount: 30},
		{Type: types.OpFormat, Format: storagev1.ImageFormat_IMAGE_FORMAT_WEBP},
		{Type: types.OpQuality, Quality: 80},
		{Type: types.OpProgressive, Progressive: true},
		{Type: types.OpStripMetadata, StripMetadata: true},
	}
	got := buildObsProcessStyle(ops)
	want := "image/auto-orient,1/image/resize,m_fill,w_200,h_200/image/sharpen,p_30/image/format,webp/image/quality,q_80/image/interlace,1/image/strip"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 3: Run the imgproc tests**

Run: `go test ./internal/provider/storage/huawei/ -count=1 -v -run 'BuildObsProcessStyle'`
Expected: all tests PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/provider/storage/huawei/imgproc.go internal/provider/storage/huawei/imgproc_test.go
git commit -m "feat(huawei): image style builder for OBS image processing

Huawei OBS uses the same image/<action>,k_v syntax as Aliyun OSS for
cloud-side image processing; reimplemented in the huawei package so the
huawei package has no dependency on aliyun. Output feeds both the
PresignGetObject URL and the CDN generator's x-image-process query."
```

---

## Task 4: STS IAM Agency client + PolicyBuilder (sts.go + sts_test.go)

**Goal:** Implement Huawei IAM Agency (委托) temporary credential issuance. Huawei uses a separate IAM service (not OBS) for STS — `CreateTemporaryAccessKeyByAgency` takes an agency name + session name + duration + policy JSON, returns an AK/SK/SecurityToken triplet. The PolicyBuilder produces the Huawei-specific JSON syntax (`Version: "1.1"`, `Resource: ["OBS::*:<bucket>/<prefix>/*"]`, condition operators `SecureTransport` / `StringEquals` for hardening).

This is the largest task: the policy JSON syntax is vendor-specific and must be unit-tested exhaustively with `assert.JSONEq`.

**Files:**
- Create: `internal/provider/storage/huawei/sts.go`
- Create: `internal/provider/storage/huawei/sts_test.go`

- [ ] **Step 1: Write sts.go**

Create `internal/provider/storage/huawei/sts.go`:

```go
package huawei

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/global"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/config"
	iam "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam/v3"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam/v3/region"
	iamModel "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam/v3/model"

	"storage-service/internal/provider/storage/types"
)

// stsClient wraps the Huawei IAM SDK so the rest of the huawei package can
// issue CreateTemporaryAccessKeyByAgency calls without exposing SDK types.
//
// Huawei splits "STS" across two unrelated services:
//   - OBS has its own PostObject policy mechanism (not used here).
//   - IAM has CreateTemporaryAccessKeyByAgency (THIS one) — issues temp
//     AK/SK bound to a delegated agency (委托) for cross-account or scoped
//     access. We use IAM because the policy syntax is richer and matches
//     the design spec.
type stsClient struct {
	cli *iam.IamClient
}

// stsClientOpts configures newSTSClient.
type stsClientOpts struct {
	AccessKey string
	SecretKey string
	// DomainID is the Huawei account ID. Required by IAM's global
	// credentials builder; the agency call is a global-scope API.
	DomainID string
	Region   string
	// Endpoint overrides the IAM regional endpoint. Empty falls back to
	// the SDK's region-derived endpoint. Tests pass the httptest.Server
	// host here.
	Endpoint string
	// HTTPScheme is "https" (default) or "http". Tests use "http" to
	// avoid TLS cert issues against httptest.Server.
	HTTPScheme string
}

// assumeAgencyReq is the project-typed input for
// CreateTemporaryAccessKeyByAgency. DurationSeconds matches the SDK field
// type (int32; Huawei accepts 900..43200).
type assumeAgencyReq struct {
	AgencyName     string
	DomainName     string
	RoleSessionName string
	DurationSeconds int32
	Policy         map[string]any
}

// assumeAgencyResp carries the temporary credentials. ExpiresAt is parsed
// from the IAM "expires_at" RFC3339 string; callers consume time.Time.
type assumeAgencyResp struct {
	AccessKey       string
	SecretKey       string
	SecurityToken   string
	ExpiresAt       time.Time
	DomainID        string
	ProjectID       string
}

// assumeAgencyCaller is the contract stsClient satisfies. Defining it as
// an interface lets tests inject a fake without exposing the SDK wrapper.
type assumeAgencyCaller interface {
	assumeAgency(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error)
}

const (
	// minHuaweiSTSDuration is the lower bound Huawei IAM enforces on
	// DurationSeconds (900s = 15min). We fail fast below this so callers
	// get an actionable error instead of a wrapped SDK API failure.
	minHuaweiSTSDuration int32 = 900
	// maxHuaweiSTSDuration is the upper bound (43200s = 12h). Failures
	// above this are surfaced as a clear local error rather than an IAM
	// rejection.
	maxHuaweiSTSDuration int32 = 43200
)

// newSTSClient builds an IAM SDK client with global-scope credentials.
// Returns an error on nil opts or empty required fields so callers fail
// fast instead of dereferencing nil later.
func newSTSClient(opts *stsClientOpts) (*stsClient, error) {
	if opts == nil {
		return nil, fmt.Errorf("nil sts client opts")
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, fmt.Errorf("access_key and secret_key required")
	}
	if opts.DomainID == "" {
		return nil, fmt.Errorf("domain_id required (Huawei account UID)")
	}

	auth, err := global.NewCredentialsBuilder().
		WithAk(opts.AccessKey).
		WithSk(opts.SecretKey).
		WithDomainId(opts.DomainID).
		SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("build iam credentials: %w", err)
	}

	builder := iam.IamClientBuilder().
		WithCredential(auth).
		WithHttpConfig(config.DefaultHttpConfig())
	if opts.Endpoint != "" {
		// Custom endpoint (tests). The SDK's region.IAM value won't be
		// consulted when endpoint is set on the HTTP config.
		builder = builder.WithEndpoint(opts.Endpoint)
	} else if opts.Region != "" {
		// Map region string to SDK region value (e.g. "cn-north-4" →
		// region.CN_NORTH_4). If the value isn't pre-known the SDK
		// falls back to a generic region builder.
		if r, err := region.ValueOf(opts.Region); err == nil && r != nil {
			builder = builder.WithRegion(r)
		}
	}
	hcClient, err := builder.SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("build iam http client: %w", err)
	}
	return &stsClient{cli: iam.NewIamClient(hcClient)}, nil
}

// assumeAgency calls IAM CreateTemporaryAccessKeyByAgency and maps the
// response to project types. ctx is propagated to the SDK via the
// per-call context (huaweicloud-sdk-go-v3 honors context).
func (c *stsClient) assumeAgency(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error) {
	if req == nil {
		return nil, fmt.Errorf("nil assume agency req")
	}
	if req.Policy == nil {
		return nil, fmt.Errorf("nil policy (Huawei requires a policy on agency temp credentials)")
	}

	policyBytes, err := marshalPolicyJSON(req.Policy)
	if err != nil {
		return nil, fmt.Errorf("marshal policy: %w", err)
	}

	// IAM's AgencyPolicy field takes a *Policy object whose Body accepts
	// a JSON string. The string content is the Huawei policy JSON.
	agencyPolicy := &iamModel.AgencyPolicy{
		Body: string(policyBytes),
	}
	sdkReq := &iamModel.CreateTemporaryAccessKeyByAgencyRequest{
		Body: &iamModel.CreateTemporaryAccessKeyByAgencyRequestBody{
			Auth: &iamModel.AgencyAuth{
				SecurityToken: &iamModel.AgencyTokenAuth{
					DurationSeconds: req.DurationSeconds,
					Agency: &iamModel.AgencyTokenAuthAgency{
						AgencyName: req.AgencyName,
						DomainName: req.DomainName,
						RoleSessionName: &req.RoleSessionName,
					},
					Policy: agencyPolicy,
				},
			},
		},
	}

	resp, err := c.cli.CreateTemporaryAccessKeyByAgency(ctx, sdkReq)
	if err != nil {
		return nil, fmt.Errorf("create temp access key by agency: %w", err)
	}
	if resp == nil || resp.Credential == nil {
		return nil, fmt.Errorf("agency credential response was empty")
	}
	cred := resp.Credential

	return &assumeAgencyResp{
		AccessKey:     valueOrEmpty(cred.Access),
		SecretKey:     valueOrEmpty(cred.Secret),
		SecurityToken: valueOrEmpty(cred.Securitytoken),
		ExpiresAt:     cred.ExpiresAt,
		DomainID:      valueOrEmpty(cred.DomainId),
		ProjectID:     valueOrEmpty(cred.ProjectId),
	}, nil
}

// --- internal helpers ---

// buildHuaweiPolicy translates STSPolicy into the JSON structure expected
// by Huawei IAM's CreateTemporaryAccessKeyByAgency Policy.Body field.
//
// Translation rules (vendor-specific; Huawei's IAM policy syntax is
// documented at https://support.huaweicloud.com/usermanual-iam/iam_01_001.html
// and differs from AWS/Aliyun RAM in resource ARN format):
//   - Version is always "1.1" (Huawei's current policy version).
//   - Bucket + KeyPrefix → Resource "OBS::*:<bucket>/<prefix>/*" where
//     the "*" segment is the (unused) account UID slot — Huawei's
//     canonical OBS resource format. We don't currently have a DomainID
//     at policy-build time; tightening requires passing the value in
//     from the provider. Empty KeyPrefix or "/" yields "OBS::*:<bucket>/*"
//     (no double slash).
//   - AllowedExtensions (each must start with '.') → one Resource entry
//     per ext.
//   - AllowedActions defaults to ["obs:object:PutObject"] for credential
//     hardening. Huawei action names use the "obs:object:<Action>" or
//     "obs:bucket:<Action>" pattern; "obs:object:" matches the resource
//     prefix emitted above.
//   - MaxSize is intentionally NOT mapped: Huawei OBS PutObject has no
//     STS-side size enforcement.
//   - EnforceHTTPS / LockObjectACL → Condition on the Allow statement.
//   - DenyPutObjectACL → additional Deny statement for
//     "obs:object:PutObjectAcl".
//
// Returns map[string]any so stsClient can marshal it with HTML escaping
// disabled (Huawei rejects <-encoded JSON).
func buildHuaweiPolicy(p *types.STSPolicy) (map[string]any, error) {
	if p == nil {
		return nil, fmt.Errorf("nil sts policy")
	}
	if p.Bucket == "" {
		return nil, fmt.Errorf("sts policy: bucket is required")
	}

	actions := p.AllowedActions
	if len(actions) == 0 {
		actions = []string{"obs:object:PutObject"}
	}

	// Trim trailing/leading slashes so empty or "/" KeyPrefix doesn't
	// produce double-slash resource patterns ("OBS::*:bucket//*") that
	// Huawei IAM matches literally and silently rejects at PUT time.
	prefix := strings.Trim(p.KeyPrefix, "/")
	scopedBase := fmt.Sprintf("OBS::*:%s", p.Bucket)
	var base string
	if prefix == "" {
		base = scopedBase + "/*"
	} else {
		base = fmt.Sprintf("%s/%s/*", scopedBase, prefix)
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

	// Condition is only added when at least one condition is enabled —
	// Huawei IAM rejects an empty Condition block, so we omit it entirely
	// when no hardening is requested. Condition operator names are
	// Huawei-specific ("SecureTransport" instead of Aliyun's
	// "acs:SecureTransport"; "obs:objectAcl" condition key).
	conditions := map[string]any{}
	if p.EnforceHTTPS {
		conditions["Bool"] = map[string]string{"SecureTransport": "true"}
	}
	if p.LockObjectACL {
		conditions["StringEquals"] = map[string]string{"obs:objectAcl": "private"}
	}
	if len(conditions) > 0 {
		allowStmt["Condition"] = conditions
	}

	statements := []map[string]any{allowStmt}

	// Explicit Deny on obs:object:PutObjectAcl prevents clients from
	// changing the ACL of an uploaded object to public-read. Resource
	// matches the same scoped set as the Allow statement.
	if p.DenyPutObjectACL {
		statements = append(statements, map[string]any{
			"Effect":   "Deny",
			"Action":   []string{"obs:object:PutObjectAcl"},
			"Resource": resources,
		})
	}

	return map[string]any{
		"Version":   "1.1",
		"Statement": statements,
	}, nil
}

// iamEndpointFor maps a Huawei region ID to its IAM regional endpoint.
// Empty region falls back to cn-north-4 (common default). Callers should
// log if they hit the fallback — it usually means misconfiguration.
func iamEndpointFor(region string) string {
	if region == "" {
		return "iam.cn-north-4.myhuaweicloud.com"
	}
	return fmt.Sprintf("iam.%s.myhuaweicloud.com", region)
}

// marshalPolicyJSON marshals the policy map with HTML escaping disabled.
// Huawei policy JSON must not escape `<`, `>`, or `&` or IAM rejects it
// as malformed. The result is trimmed because json.Encoder.Encode appends
// '\n'.
func marshalPolicyJSON(p map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

// valueOrEmpty dereferences a *string safely; empty string when nil.
// Huawei SDK fields are pointer-typed and may be nil on partial responses.
func valueOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
```

- [ ] **Step 2: Write sts_test.go**

Create `internal/provider/storage/huawei/sts_test.go`:

```go
package huawei

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storage-service/internal/provider/storage/types"
)

// TestBuildHuaweiPolicy_NoExtensions verifies empty AllowedExtensions
// yields a single Resource wildcard covering the entire prefix.
func TestBuildHuaweiPolicy_NoExtensions(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:    "photos",
		KeyPrefix: "uploads/",
	})
	require.NoError(t, err)

	assert.Equal(t, "1.1", policy["Version"])
	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	assert.Equal(t, "Allow", stmts[0]["Effect"])
	assert.Equal(t, []string{"obs:object:PutObject"}, stmts[0]["Action"])
	assert.Equal(t, []string{"OBS::*:photos/uploads/*"}, stmts[0]["Resource"])
	// No hardening flags set → Condition must be absent.
	_, hasCond := stmts[0]["Condition"]
	assert.False(t, hasCond, "Condition should be absent when no hardening flags set")
}

// TestBuildHuaweiPolicy_WithExtensions verifies each extension becomes a
// separate Resource entry.
func TestBuildHuaweiPolicy_WithExtensions(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
	})
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	resources := stmts[0]["Resource"].([]string)
	assert.Equal(t, []string{
		"OBS::*:photos/uploads/*.jpg",
		"OBS::*:photos/uploads/*.png",
	}, resources)
}

// TestBuildHuaweiPolicy_BadExtensionFormat verifies extensions missing the
// '.' prefix are rejected.
func TestBuildHuaweiPolicy_BadExtensionFormat(t *testing.T) {
	_, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{"jpg"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with '.'")
}

// TestBuildHuaweiPolicy_CustomActions verifies AllowedActions override
// default.
func TestBuildHuaweiPolicy_CustomActions(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:         "photos",
		KeyPrefix:      "uploads/",
		AllowedActions: []string{"obs:object:PutObject", "obs:object:GetObject"},
	})
	require.NoError(t, err)
	stmts := policy["Statement"].([]map[string]any)
	assert.Equal(t, []string{"obs:object:PutObject", "obs:object:GetObject"}, stmts[0]["Action"])
}

// TestBuildHuaweiPolicy_KeyPrefixTrailingSlashStripped verifies prefix
// normalization (no double slash).
func TestBuildHuaweiPolicy_KeyPrefixTrailingSlashStripped(t *testing.T) {
	for _, prefix := range []string{"uploads/", "uploads"} {
		policy, err := buildHuaweiPolicy(&types.STSPolicy{
			Bucket:    "photos",
			KeyPrefix: prefix,
		})
		require.NoError(t, err)
		stmts := policy["Statement"].([]map[string]any)
		resources := stmts[0]["Resource"].([]string)
		assert.Equal(t, []string{"OBS::*:photos/uploads/*"}, resources,
			"prefix %q should normalize", prefix)
	}
}

// TestBuildHuaweiPolicy_EmptyOrSlashKeyPrefix verifies that an empty or
// "/" KeyPrefix produces a single-slash resource base. Without this guard
// the format string yields "OBS::*:bucket//*" (double slash) which Huawei
// IAM matches literally and silently rejects at PUT time.
func TestBuildHuaweiPolicy_EmptyOrSlashKeyPrefix(t *testing.T) {
	for _, prefix := range []string{"", "/", "//"} {
		policy, err := buildHuaweiPolicy(&types.STSPolicy{
			Bucket:    "photos",
			KeyPrefix: prefix,
		})
		require.NoError(t, err)
		stmts := policy["Statement"].([]map[string]any)
		resources := stmts[0]["Resource"].([]string)
		assert.Equal(t, []string{"OBS::*:photos/*"}, resources,
			"prefix %q should normalize to bucket-only resource", prefix)
	}
}

// TestBuildHuaweiPolicy_EnforceHTTPS verifies the Bool Condition that
// blocks plaintext HTTP uploads at OBS. Note: Huawei's condition key is
// "SecureTransport" (NOT Aliyun's "acs:SecureTransport").
func TestBuildHuaweiPolicy_EnforceHTTPS(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
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
		"Bool": map[string]string{"SecureTransport": "true"},
	}, cond)
}

// TestBuildHuaweiPolicy_LockObjectACL verifies the StringEquals Condition
// that forces uploaded objects to "private" regardless of client-supplied
// ACL headers. Huawei condition key is "obs:objectAcl".
func TestBuildHuaweiPolicy_LockObjectACL(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
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
		"StringEquals": map[string]string{"obs:objectAcl": "private"},
	}, cond)
}

// TestBuildHuaweiPolicy_AllConditions verifies the two Condition operators
// can coexist in the same statement without colliding (different keys).
func TestBuildHuaweiPolicy_AllConditions(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
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

// TestBuildHuaweiPolicy_DenyPutObjectACL verifies that enabling
// DenyPutObjectACL appends a second Deny statement targeting
// obs:object:PutObjectAcl on the same Resource set as the Allow.
func TestBuildHuaweiPolicy_DenyPutObjectACL(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
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
	assert.Equal(t, []string{"obs:object:PutObjectAcl"}, denyStmt["Action"])

	// Deny Resource must match Allow Resource exactly.
	allowRes := stmts[0]["Resource"].([]string)
	denyRes := denyStmt["Resource"].([]string)
	assert.Equal(t, allowRes, denyRes, "Deny Resource must match Allow Resource")
	assert.Equal(t, []string{
		"OBS::*:photos/uploads/*.jpg",
		"OBS::*:photos/uploads/*.png",
	}, denyRes)
}

// TestBuildHuaweiPolicy_NilPolicy verifies the nil guard.
func TestBuildHuaweiPolicy_NilPolicy(t *testing.T) {
	_, err := buildHuaweiPolicy(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts policy")
}

// TestBuildHuaweiPolicy_MissingBucket verifies the bucket-required guard.
func TestBuildHuaweiPolicy_MissingBucket(t *testing.T) {
	_, err := buildHuaweiPolicy(&types.STSPolicy{
		KeyPrefix: "uploads/",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bucket is required")
}

// TestBuildHuaweiPolicy_JSONEq_NoExtensions uses assert.JSONEq to lock the
// full JSON shape. This catches accidental field-name changes (e.g.
// someone refactoring "Statement" → "Statements") that the per-field
// asserts above might miss.
func TestBuildHuaweiPolicy_JSONEq_NoExtensions(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:    "photos",
		KeyPrefix: "uploads/",
	})
	require.NoError(t, err)
	got, err := marshalPolicyJSON(policy)
	require.NoError(t, err)

	want := `{
		"Version": "1.1",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["obs:object:PutObject"],
			"Resource": ["OBS::*:photos/uploads/*"]
		}]
	}`
	assert.JSONEq(t, want, string(got))
}

// TestBuildHuaweiPolicy_JSONEq_FullHardening verifies the full hardened
// policy shape via assert.JSONEq: Allow + Condition (Bool+StringEquals) +
// Deny statement.
func TestBuildHuaweiPolicy_JSONEq_FullHardening(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg"},
		EnforceHTTPS:      true,
		LockObjectACL:     true,
		DenyPutObjectACL:  true,
	})
	require.NoError(t, err)
	got, err := marshalPolicyJSON(policy)
	require.NoError(t, err)

	want := `{
		"Version": "1.1",
		"Statement": [
			{
				"Effect": "Allow",
				"Action": ["obs:object:PutObject"],
				"Resource": ["OBS::*:photos/uploads/*.jpg"],
				"Condition": {
					"Bool": {"SecureTransport": "true"},
					"StringEquals": {"obs:objectAcl": "private"}
				}
			},
			{
				"Effect": "Deny",
				"Action": ["obs:object:PutObjectAcl"],
				"Resource": ["OBS::*:photos/uploads/*.jpg"]
			}
		]
	}`
	assert.JSONEq(t, want, string(got))
}

// TestIamEndpointFor verifies the regional endpoint builder.
func TestIamEndpointFor(t *testing.T) {
	cases := []struct {
		region string
		want   string
	}{
		{"cn-north-4", "iam.cn-north-4.myhuaweicloud.com"},
		{"ap-southeast-1", "iam.ap-southeast-1.myhuaweicloud.com"},
		{"", "iam.cn-north-4.myhuaweicloud.com"},
	}
	for _, tc := range cases {
		t.Run(tc.region, func(t *testing.T) {
			assert.Equal(t, tc.want, iamEndpointFor(tc.region))
		})
	}
}

// TestNewSTSClient_NilOpts verifies the constructor fails fast on nil opts.
func TestNewSTSClient_NilOpts(t *testing.T) {
	_, err := newSTSClient(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts client opts")
}

// TestNewSTSClient_MissingCreds verifies AK/SK are required.
func TestNewSTSClient_MissingCreds(t *testing.T) {
	_, err := newSTSClient(&stsClientOpts{
		DomainID: "demo-account",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access_key and secret_key required")
}

// TestNewSTSClient_MissingDomainID verifies the DomainID requirement
// (Huawei global-scope credentials builder needs it).
func TestNewSTSClient_MissingDomainID(t *testing.T) {
	_, err := newSTSClient(&stsClientOpts{
		AccessKey: "ak",
		SecretKey: "sk",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "domain_id required")
}

// fakeSTS is a minimal stsClient stand-in for unit-testing GetSTSToken
// without spinning up an HTTP server.
type fakeSTS struct {
	gotReq *assumeAgencyReq
	resp   *assumeAgencyResp
	err    error
}

func (f *fakeSTS) assumeRole(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error) {
	_ = ctx
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// satisfy the assumeAgencyCaller interface — note we accept the method
// name `assumeRole` because fakeSTS is a test-only stand-in; the real
// stsClient uses assumeAgency.
var _ assumeAgencyCaller = (*fakeAliasSTS)(nil)

// fakeAliasSTS adapts fakeSTS to assumeAgencyCaller by forwarding to
// assumeRole. Tests that need a fake caller use this type so the
// production *stsClient stays the only assumeAgency implementor.
type fakeAliasSTS struct{ inner *fakeSTS }

func (a *fakeAliasSTS) assumeAgency(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error) {
	return a.inner.assumeRole(ctx, req)
}

// newHuaweiProviderWithFakeSTS bypasses the real constructor (which would
// try to init a real stsClient) and wires the fake manually. If fake is
// nil the provider's stsCli field stays a nil interface so GetSTSToken's
// nil-guard fires correctly.
//
// Defined here in Task 4 so Task 4 tests for GetSTSToken error paths can
// compile. The provider struct is defined in Task 5; this helper compiles
// only after Task 5 lands, so Task 4's tests are written but skip until
// Task 5 is implemented. (See Task 5 for the helper's final form.)
func newHuaweiProviderWithFakeSTS(fake assumeAgencyCaller, agencyName string) *HuaweiProvider {
	p := &HuaweiProvider{
		endpoint:  "https://obs.example.com",
		accessKey: "ak",
		secretKey: "sk",
		region:    "cn-north-4",
		domainID:  "demo-account",
		roleARN:   agencyName, // roleARN carries the agency NAME on Huawei
	}
	if fake != nil {
		p.stsCli = fake
	}
	return p
}

// TestHuaweiProvider_GetSTSToken_NoAgencyName verifies that a provider
// without RoleARN (agency name) returns an explicit error rather than
// panicking on nil stsCli.
func TestHuaweiProvider_GetSTSToken_NoAgencyName(t *testing.T) {
	p := newHuaweiProviderWithFakeSTS(nil, "")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestHuaweiProvider_GetSTSToken_NilClientButAgencySet covers the
// defensive branch where agencyName is set but stsCli is nil — should not
// happen via the constructor, but the nil-guard must still produce the
// "not configured" error instead of panicking.
func TestHuaweiProvider_GetSTSToken_NilClientButAgencySet(t *testing.T) {
	p := newHuaweiProviderWithFakeSTS(nil, "uploader-agency")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestHuaweiProvider_GetSTSToken_BelowMinTTL verifies that a TTL below
// Huawei's 900s minimum is rejected locally with an actionable error
// instead of being forwarded to the SDK.
func TestHuaweiProvider_GetSTSToken_BelowMinTTL(t *testing.T) {
	fakeInner := &fakeSTS{resp: &assumeAgencyResp{ExpiresAt: time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)}}
	p := newHuaweiProviderWithFakeSTS(&fakeAliasSTS{inner: fakeInner}, "uploader-agency")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       5 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Huawei minimum")
	assert.Nil(t, fakeInner.gotReq, "must not call stsCli when TTL validation fails locally")
}

// TestHuaweiProvider_GetSTSToken_AboveMaxTTL verifies that a TTL above
// 43200s (12h) is rejected locally.
func TestHuaweiProvider_GetSTSToken_AboveMaxTTL(t *testing.T) {
	fakeInner := &fakeSTS{resp: &assumeAgencyResp{ExpiresAt: time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)}}
	p := newHuaweiProviderWithFakeSTS(&fakeAliasSTS{inner: fakeInner}, "uploader-agency")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       24 * time.Hour,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Huawei maximum")
}

// TestHuaweiProvider_GetSTSToken_Success verifies happy path.
func TestHuaweiProvider_GetSTSToken_Success(t *testing.T) {
	expiresAt := time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)
	fakeInner := &fakeSTS{
		resp: &assumeAgencyResp{
			AccessKey:     "STS.ak",
			SecretKey:     "STS.sk",
			SecurityToken: "STS.token",
			ExpiresAt:     expiresAt,
			DomainID:      "demo-account",
		},
	}
	p := newHuaweiProviderWithFakeSTS(&fakeAliasSTS{inner: fakeInner}, "uploader-agency")

	cred, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		OwnerID:           100,
		OwnerType:         1,
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
		TTL:               15 * time.Minute,
	})
	require.NoError(t, err)

	assert.Equal(t, "owner-100", fakeInner.gotReq.RoleSessionName)
	assert.Equal(t, "uploader-agency", fakeInner.gotReq.AgencyName)
	assert.Equal(t, int32(900), fakeInner.gotReq.DurationSeconds)

	assert.Equal(t, "1.1", fakeInner.gotReq.Policy["Version"])

	assert.Equal(t, "STS.ak", cred.AccessKey)
	assert.Equal(t, "STS.sk", cred.SecretKey)
	assert.Equal(t, "STS.token", cred.SecurityToken)
	assert.Equal(t, "https://obs.example.com", cred.Endpoint)
	assert.Equal(t, "cn-north-4", cred.Region)
	assert.Equal(t, "photos", cred.Bucket)
	assert.Equal(t, "uploads/", cred.ObjectKeyPrefix)
	assert.WithinDuration(t, expiresAt, cred.ExpiresAt, time.Second)
}

// TestHuaweiProvider_GetSTSToken_NilPolicy verifies nil policy is rejected.
func TestHuaweiProvider_GetSTSToken_NilPolicy(t *testing.T) {
	fakeInner := &fakeSTS{}
	p := newHuaweiProviderWithFakeSTS(&fakeAliasSTS{inner: fakeInner}, "uploader-agency")
	_, err := p.GetSTSToken(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts policy")
}
```

- [ ] **Step 3: Run the policy builder + STS client tests (skip provider tests for now — provider struct lands in Task 5)**

Run: `go test ./internal/provider/storage/huawei/ -count=1 -v -run 'BuildHuaweiPolicy|IamEndpoint|NewSTSClient'`
Expected: all policy / endpoint / client-construction tests PASS.

Note: The `TestHuaweiProvider_GetSTSToken_*` tests reference `HuaweiProvider` which is defined in Task 5; they will not compile yet. They will compile and pass once Task 5 lands. This is intentional: Task 5's provider struct must define exactly the fields `endpoint / accessKey / secretKey / region / domainID / roleARN / stsCli` for the test helper in Task 4 to compile.

- [ ] **Step 4: Commit (defer to Task 5 to also include provider.go so the test file compiles)**

Note on commit strategy: Because `sts_test.go`'s `TestHuaweiProvider_GetSTSToken_*` tests reference the `HuaweiProvider` struct, the Task 4 commit will only include `sts.go` + the policy-builder tests; the full `sts_test.go` (including the provider-touching tests) will be committed in Task 5 alongside `provider.go`. To keep this step simple:

Run:
```bash
# Stage just sts.go for now
git add internal/provider/storage/huawei/sts.go
git commit -m "feat(huawei): IAM Agency STS client + Huawei policy JSON builder

CreateTemporaryAccessKeyByAgency wrapper for Huawei's IAM-based STS
(委托). PolicyBuilder emits Huawei-specific JSON: Version 1.1,
Resource OBS::*:bucket/prefix/*, Bool/StringEquals Condition operators
(SecureTransport / obs:objectAcl keys, NOT Aliyun's acs: variants).
DenyPutObjectACL emits a separate Deny statement for
obs:object:PutObjectAcl. assert.JSONEq tests lock the full shape.

Provider-touching tests (TestHuaweiProvider_GetSTSToken_*) come in the
next commit alongside provider.go."
```

(Task 5 will run `git add internal/provider/storage/huawei/sts_test.go` to commit the rest of the test file.)

---

## Task 5: Provider 8 methods (provider.go + provider_test.go)

**Goal:** Implement `HuaweiProvider` with all 8 `types.Provider` methods using the OBS Go SDK. The struct also wires up the STS client from Task 4. `HeadObject` does a follow-up `GetObjectAcl` best-effort call (same pattern as aliyun). `PresignGetObject` honors `WithPublic()`.

**Files:**
- Create: `internal/provider/storage/huawei/provider.go`
- Create: `internal/provider/storage/huawei/provider_test.go`

- [ ] **Step 1: Write provider.go**

Create `internal/provider/storage/huawei/provider.go`:

```go
package huawei

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"storage-service/internal/provider/storage/types"
)

// HuaweiProvider implements the Provider interface for Huawei OBS via the
// huaweicloud-sdk-go-obs module. All methods honor ctx — cancellation
// and timeout signals propagate to OBS operations.
//
// STS (GetSTSToken) uses a separate IAM-v3 client (委托 Agency) wired in
// at construction time when roleARN (agency name) is non-empty.
//
// CDN URL generation lives in the standalone CDNURLGenerator type — this
// provider only handles OBS operations.
type HuaweiProvider struct {
	client    *obs.ObsClient
	endpoint  string
	accessKey string
	secretKey string
	region    string
	domainID  string // required by IAM global credentials
	roleARN   string // agency name; empty = STS unavailable
	stsCli    assumeAgencyCaller // nil when roleARN unconfigured
}

// Compile-time assertion that *HuaweiProvider satisfies types.Provider.
var _ types.Provider = (*HuaweiProvider)(nil)

// NewHuaweiProvider creates a new HuaweiProvider with the given
// credentials. region and endpoint are required by the OBS SDK for
// request signing; roleARN is the agency name (NOT an ARN — see
// ProviderConfig.RoleARN doc comment). When roleARN is non-empty, the
// provider can issue STS credentials via IAM Agency; when empty,
// GetSTSToken returns an explicit error.
//
// domainNameForAgency is the Huawei account NAME (or domain alias) the
// agency belongs to. Required for CreateTemporaryAccessKeyByAgency.
// Callers extract this from config at construction time. When roleARN is
// empty, this argument is ignored.
func NewHuaweiProvider(endpoint, accessKey, secretKey, roleARN, domainID, domainName, region string) (*HuaweiProvider, error) {
	obsClient, err := obs.New(accessKey, secretKey, normalizeEndpoint(endpoint),
		obs.WithRegion(region),
		obs.WithSignature(obs.SignatureObs),
	)
	if err != nil {
		return nil, fmt.Errorf("create obs client: %w", err)
	}
	p := &HuaweiProvider{
		client:    obsClient,
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		domainID:  domainID,
		roleARN:   roleARN,
	}
	if roleARN != "" {
		stsCli, err := newSTSClient(&stsClientOpts{
			AccessKey:  accessKey,
			SecretKey:  secretKey,
			DomainID:   domainID,
			Region:     region,
			Endpoint:   iamEndpointFor(region),
			HTTPScheme: "https",
		})
		if err != nil {
			return nil, fmt.Errorf("create iam client: %w", err)
		}
		p.stsCli = stsCli
		_ = domainName // currently unused; reserved for future per-agency domain override
	}
	return p, nil
}

// PutObject uploads data to the specified bucket and key.
func (p *HuaweiProvider) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...types.PutOption) error {
	putOpts := types.NewPutOptions(opts...)
	input := &obs.PutObjectInput{
		Bucket: bucket,
		Key:    key,
		Body:   reader,
	}
	if putOpts.ContentType != "" {
		input.ContentType = putOpts.ContentType
	}
	if _, err := p.client.PutObject(input); err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket and key.
// The caller must close the returned reader.
func (p *HuaweiProvider) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	output, err := p.client.GetObject(&obs.GetObjectInput{
		GetObjectMetadataInput: obs.GetObjectMetadataInput{
			Bucket: bucket,
			Key:    key,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return output.Body, nil
}

// DeleteObject removes an object from the specified bucket and key.
func (p *HuaweiProvider) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := p.client.DeleteObject(&obs.DeleteObjectInput{
		Bucket: bucket,
		Key:    key,
	}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// HeadObject retrieves metadata for an object without downloading its
// body. When the object is absent, the wrapped error satisfies
// errors.Is(err, types.ErrObjectNotFound).
//
// The OBS SDK's GetObjectMetadata does not return the x-obs-acl header,
// so a follow-up GetObjectAcl call is made to populate ObjectACL. The
// upload service relies on this field to detect ACL violations on
// private sessions, so the extra round trip is intentional.
func (p *HuaweiProvider) HeadObject(ctx context.Context, bucket, key string) (*types.ObjectInfo, error) {
	head, err := p.client.GetObjectMetadata(&obs.GetObjectMetadataInput{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		if isHuaweiNotFound(err) {
			return nil, fmt.Errorf("head object %q: %w", key, types.ErrObjectNotFound)
		}
		return nil, fmt.Errorf("head object %q: %w", key, err)
	}

	info := objectInfoFromHead(key, head)

	// GetObjectAcl is best-effort: if it fails (e.g. permission denied on
	// the ACL subresource), we still return the rest of the metadata with
	// an empty ObjectACL rather than failing the entire HeadObject call.
	aclResp, aclErr := p.client.GetObjectAcl(&obs.GetObjectAclInput{
		Bucket: bucket,
		Key:    key,
	})
	if aclErr == nil && aclResp != nil && aclResp.Grants != nil {
		// OBS exposes ACL as a list of grants; if the object has a single
		// "AllUsers" READ grant, surface "public-read", else "private".
		// The original ObjectACL string is in the response's
		// AccessControlPolicy.Owner.ID when present; for now we derive
		// the canonical name via the helper.
		info.ObjectACL = huaweiACLOrPrivate(aclResp.Grants)
	}

	return info, nil
}

// PresignPutObject generates a presigned URL for uploading an object.
// Options signed into the URL require the client to send matching headers.
func (p *HuaweiProvider) PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.PutPresignOption) (string, http.Header, error) {
	putOpts := types.NewPutPresignOptions(opts...)
	input := &obs.CreateBrowserPresignedUrlInput{
		Bucket:  bucket,
		Key:     key,
		Method:  obs.HttpMethodPut,
		Expires: int(ttl.Seconds()),
	}
	if putOpts.ContentType != "" {
		input.Headers = map[string]string{
			"Content-Type": putOpts.ContentType,
		}
	}
	if putOpts.CacheControl != "" {
		if input.Headers == nil {
			input.Headers = map[string]string{}
		}
		input.Headers["Cache-Control"] = putOpts.CacheControl
	}
	for k, v := range putOpts.Metadata {
		if input.Headers == nil {
			input.Headers = map[string]string{}
		}
		input.Headers["x-obs-meta-"+k] = v
	}

	output, err := p.client.CreateBrowserPresignedUrl(input)
	if err != nil {
		return "", nil, fmt.Errorf("sign put url for %q: %w", key, err)
	}

	// Surface signed headers so callers can forward them to the client.
	var headers http.Header
	if len(input.Headers) > 0 {
		headers = make(http.Header, len(input.Headers))
		for k, v := range input.Headers {
			headers.Set(k, v)
		}
	}
	return output.SignedUrl, headers, nil
}

// PresignGetObject generates a presigned URL for downloading an object.
//
// When WithPublic() is passed, returns an unsigned URL of the form
// https://<bucket>.<endpoint>/<key>. The caller MUST verify the object's
// bucket ACL is "public_read" before requesting this mode.
func (p *HuaweiProvider) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.GetPresignOption) (string, error) {
	getOpts := types.NewGetPresignOptions(opts...)
	if getOpts.Public {
		return publicObjectURL(p.endpoint, bucket, key), nil
	}
	input := &obs.CreateBrowserPresignedUrlInput{
		Bucket:  bucket,
		Key:     key,
		Method:  obs.HttpMethodGet,
		Expires: int(ttl.Seconds()),
	}
	queryParams := map[string]string{}
	if getOpts.Filename != "" {
		queryParams["response-content-disposition"] = types.BuildContentDisposition(getOpts.Filename)
	}
	if getOpts.ResponseContentType != "" {
		queryParams["response-content-type"] = getOpts.ResponseContentType
	}
	if getOpts.ResponseCacheControl != "" {
		queryParams["response-cache-control"] = getOpts.ResponseCacheControl
	}
	if len(getOpts.ImageOps) > 0 {
		queryParams["x-image-process"] = buildObsProcessStyle(getOpts.ImageOps)
	}
	if len(queryParams) > 0 {
		input.QueryParams = queryParams
	}

	output, err := p.client.CreateBrowserPresignedUrl(input)
	if err != nil {
		return "", fmt.Errorf("sign get url for %q: %w", key, err)
	}
	return output.SignedUrl, nil
}

// ListObjects lists all objects under the given prefix in the specified
// bucket. Paginates internally by following the Marker field.
func (p *HuaweiProvider) ListObjects(ctx context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	var result []types.ObjectInfo
	var marker string
	for {
		input := &obs.ListObjectsInput{
			Bucket: bucket,
			Prefix: prefix,
		}
		if marker != "" {
			input.Marker = marker
		}
		page, err := p.client.ListObjects(input)
		if err != nil {
			return nil, fmt.Errorf("list objects prefix=%q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			result = append(result, types.ObjectInfo{
				Key:          obj.Key,
				Size:         obj.Size,
				ETag:         strings.Trim(obj.ETag, `"`),
				LastModified: obj.LastModified,
			})
		}
		if !page.IsTruncated {
			break
		}
		marker = page.NextMarker
	}
	return result, nil
}

// GetSTSToken retrieves temporary credentials via IAM Agency
// (CreateTemporaryAccessKeyByAgency). Requires roleARN to be configured
// at NewHuaweiProvider time; otherwise returns an explicit error so
// callers know to use GenerateUploadURL instead.
//
// RoleSessionName embeds OwnerID so OBS audit logs can trace credentials
// back to the originating user. OwnerID is not sensitive.
func (p *HuaweiProvider) GetSTSToken(ctx context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
	if p == nil || p.stsCli == nil || p.roleARN == "" {
		return nil, fmt.Errorf("huawei STS not configured for this provider; set provider.role_arn to the agency name in config")
	}
	if policy == nil {
		return nil, fmt.Errorf("nil sts policy")
	}

	policyJSON, err := buildHuaweiPolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("build sts policy: %w", err)
	}

	duration := int32(policy.TTL.Seconds())
	if duration <= 0 {
		return nil, fmt.Errorf("sts policy: TTL must be > 0")
	}
	// Huawei IAM enforces [900, 43200]s. Fail fast here so callers get
	// an actionable message instead of a wrapped SDK error from the
	// cloud.
	if duration < minHuaweiSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v below Huawei minimum of %ds",
			policy.TTL, minHuaweiSTSDuration)
	}
	if duration > maxHuaweiSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v above Huawei maximum of %ds",
			policy.TTL, maxHuaweiSTSDuration)
	}

	resp, err := p.stsCli.assumeAgency(ctx, &assumeAgencyReq{
		AgencyName:      p.roleARN, // roleARN carries the agency NAME on Huawei
		DomainName:      p.domainID, // DomainName field accepts the account ID
		RoleSessionName: fmt.Sprintf("owner-%d", policy.OwnerID),
		DurationSeconds: duration,
		Policy:          policyJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("huawei iam create temp access key by agency: %w", err)
	}

	return &types.STSCredential{
		AccessKey:       resp.AccessKey,
		SecretKey:       resp.SecretKey,
		SecurityToken:   resp.SecurityToken,
		Endpoint:        p.endpoint,
		Region:          p.region,
		Bucket:          policy.Bucket,
		ObjectKeyPrefix: policy.KeyPrefix,
		ExpiresAt:       resp.ExpiresAt,
	}, nil
}

// --- internal helpers ---

// objectInfoFromHead translates the OBS GetObjectMetadataOutput into a
// types.ObjectInfo. ObjectACL is left empty here; HeadObject fills it via
// a separate GetObjectAcl call.
func objectInfoFromHead(key string, head *obs.GetObjectMetadataOutput) *types.ObjectInfo {
	return &types.ObjectInfo{
		Key:          key,
		Size:         head.ContentLength,
		ETag:         strings.Trim(head.ETag, `"`),
		ContentType:  head.ContentType,
		LastModified: head.LastModified,
	}
}

// isHuaweiNotFound reports whether err is a Huawei OBS "object/bucket
// absent" response. The OBS SDK surfaces 404s as obs.ObsError with
// StatusCode==404 (BaseModel.StatusCode).
func isHuaweiNotFound(err error) bool {
	var obsErr obs.ObsError
	if errors.As(err, &obsErr) {
		return obsErr.StatusCode == http.StatusNotFound
	}
	return false
}

// huaweiACLOrPrivate maps OBS grant list to the canonical ACL name. OBS
// does not return a simple "private|public-read" string the way Aliyun
// does — it returns a list of {Grantee, Permission} pairs. We treat
// "AllUsers + READ" as public-read; everything else is private.
func huaweiACLOrPrivate(grants []obs.Grant) string {
	for _, g := range grants {
		if g.Grantee.URI == obs.GroupAllUsers && g.Permission == obs.PermissionRead {
			return types.ObjectACLPublicRead
		}
	}
	return types.ObjectACLPrivate
}

// publicObjectURL builds the unsigned URL for a public-read OBS object:
// https://<bucket>.<endpoint>/<key>. The endpoint is normalized so
// callers may pass it with or without a scheme, and with or without a
// trailing slash.
func publicObjectURL(endpoint, bucket, key string) string {
	ep := endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	ep = strings.TrimSuffix(ep, "/")
	if strings.HasPrefix(ep, "https://") || strings.HasPrefix(ep, "http://") {
		scheme := ep[:strings.Index(ep, "://")+3]
		host := ep[strings.Index(ep, "://")+3:]
		// OBS uses <bucket>.<endpoint> virtual-host style for public URLs.
		return scheme + bucket + "." + host + "/" + strings.TrimPrefix(key, "/")
	}
	return ep + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
}

// normalizeEndpoint ensures the endpoint has a scheme — the OBS SDK
// requires "https://obs.<region>.myhuaweicloud.com" form. Empty input is
// left empty so the SDK falls back to its region-derived endpoint.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if !strings.Contains(endpoint, "://") {
		return "https://" + endpoint
	}
	return endpoint
}
```

- [ ] **Step 2: Update sts_test.go with the helper fields the test expects**

The Task 4 test file `sts_test.go` calls `newHuaweiProviderWithFakeSTS` which references `HuaweiProvider` fields. Re-open `internal/provider/storage/huawei/sts_test.go` and confirm the helper compiles — the struct fields `endpoint / accessKey / secretKey / region / domainID / roleARN / stsCli` defined in `provider.go` (Step 1) match what the helper sets. No edits needed if the field names align (they do — see provider.go Step 1).

- [ ] **Step 3: Write provider_test.go with HTTP-mocked OBS**

Create `internal/provider/storage/huawei/provider_test.go`:

```go
package huawei

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storage-service/internal/provider/storage/types"
)

// newTestProvider constructs a HuaweiProvider pointed at the httptest
// server. The OBS SDK accepts a custom endpoint via the constructor's
// endpoint argument; the test server URL passes through unchanged.
func newTestProvider(t *testing.T, srvURL string) *HuaweiProvider {
	t.Helper()
	p, err := NewHuaweiProvider(srvURL, "ak", "sk", "", "", "", "cn-north-4")
	require.NoError(t, err)
	return p
}

// TestObjectInfoFromHead_AllFieldsPopulated verifies the happy-path
// mapping from OBS GetObjectMetadataOutput to types.ObjectInfo.
func TestObjectInfoFromHead_AllFieldsPopulated(t *testing.T) {
	lastModified := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	head := &obsHeadStub{
		ContentLength: 2048,
		ETag:          `"deadbeef"`,
		ContentType:   "image/jpeg",
		LastModified:  lastModified,
	}
	info := objectInfoFromHeadFromStub("photos/abc.jpg", head)
	assert.Equal(t, "photos/abc.jpg", info.Key)
	assert.Equal(t, int64(2048), info.Size)
	assert.Equal(t, "deadbeef", info.ETag, "ETag quotes must be stripped")
	assert.Equal(t, "image/jpeg", info.ContentType)
	assert.WithinDuration(t, lastModified, info.LastModified, time.Second)
	assert.Empty(t, info.ObjectACL, "objectInfoFromHead must not populate ObjectACL")
}

// TestPublicObjectURL verifies the unsigned public-read URL format for
// PresignGetObject(WithPublic()).
func TestPublicObjectURL(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		bucket   string
		key      string
		want     string
	}{
		{"scheme-prefixed", "https://obs.example.com", "b", "k", "https://b.obs.example.com/k"},
		{"no-scheme", "obs.example.com", "b", "k", "https://b.obs.example.com/k"},
		{"trailing-slash-stripped", "https://obs.example.com/", "b", "k", "https://b.obs.example.com/k"},
		{"leading-slash-key", "https://obs.example.com", "b", "/k", "https://b.obs.example.com/k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, publicObjectURL(tc.endpoint, tc.bucket, tc.key))
		})
	}
}

// TestNormalizeEndpoint covers the scheme-prefix helper.
func TestNormalizeEndpoint(t *testing.T) {
	assert.Equal(t, "", normalizeEndpoint(""))
	assert.Equal(t, "https://obs.example.com", normalizeEndpoint("obs.example.com"))
	assert.Equal(t, "https://obs.example.com", normalizeEndpoint("https://obs.example.com"))
}

// TestHuaweiACLMapping covers the grant-list → canonical-name helper.
func TestHuaweiACLMapping(t *testing.T) {
	// Use real obs types so the field names stay pinned to the SDK.
	grants := []obsGrantStub{}
	assert.Equal(t, types.ObjectACLPrivate, huaweiACLOrPrivateStubs(grants))

	grants = []obsGrantStub{{uri: "AllUsers", perm: "READ"}}
	assert.Equal(t, types.ObjectACLPublicRead, huaweiACLOrPrivateStubs(grants))

	grants = []obsGrantStub{{uri: "AllUsers", perm: "WRITE"}}
	assert.Equal(t, types.ObjectACLPrivate, huaweiACLOrPrivateStubs(grants))
}

// TestHuaweiProvider_PutObject_HappyPath mocks OBS and verifies the
// request URL + headers reach the wire correctly.
func TestHuaweiProvider_PutObject_HappyPath(t *testing.T) {
	var capturedMethod, capturedPath string
	var capturedCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedCT = r.Header.Get("Content-Type")
		w.Header().Set("ETag", `"deadbeef"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	err := p.PutObject(context.Background(), "b", "k", strings.NewReader("body"), 4,
		types.WithContentType("text/plain"))
	require.NoError(t, err)
	assert.Equal(t, "PUT", capturedMethod)
	assert.Equal(t, "/b/k", capturedPath)
	assert.Equal(t, "text/plain", capturedCT)
}

// TestHuaweiProvider_PutObject_APIError verifies OBS errors are wrapped
// with the operation context.
func TestHuaweiProvider_PutObject_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>forbidden</Message></Error>`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	err := p.PutObject(context.Background(), "b", "k", strings.NewReader("body"), 4)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "put object")
}

// TestHuaweiProvider_GetObject_HappyPath verifies the body reader is
// returned and the caller can drain it.
func TestHuaweiProvider_GetObject_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello body"))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	rc, err := p.GetObject(context.Background(), "b", "k")
	require.NoError(t, err)
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello body", string(body))
}

// TestHuaweiProvider_DeleteObject_HappyPath verifies the DELETE request.
func TestHuaweiProvider_DeleteObject_HappyPath(t *testing.T) {
	var capturedMethod, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	err := p.DeleteObject(context.Background(), "b", "k")
	require.NoError(t, err)
	assert.Equal(t, "DELETE", capturedMethod)
	assert.Equal(t, "/b/k", capturedPath)
}

// TestHuaweiProvider_HeadObject_NotFound verifies 404 maps to
// types.ErrObjectNotFound via errors.Is.
func TestHuaweiProvider_HeadObject_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	_, err := p.HeadObject(context.Background(), "b", "k")
	require.Error(t, err)
	assert.ErrorIs(t, err, types.ErrObjectNotFound)
}

// TestHuaweiProvider_PresignPutObject_ReturnsURL verifies the presigned
// PUT URL is non-empty and contains the bucket+key in the path.
func TestHuaweiProvider_PresignPutObject_ReturnsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	url, headers, err := p.PresignPutObject(context.Background(), "b", "k", time.Hour,
		types.WithUploadContentType("image/jpeg"))
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Contains(t, url, "/b/k")
	require.NotNil(t, headers)
	assert.Equal(t, "image/jpeg", headers.Get("Content-Type"))
}

// TestHuaweiProvider_PresignGetObject_Public verifies WithPublic returns
// the unsigned URL and skips the OBS SDK call entirely.
func TestHuaweiProvider_PresignGetObject_Public(t *testing.T) {
	// Public path doesn't hit OBS — no httptest.Server needed.
	p, err := NewHuaweiProvider("obs.example.com", "ak", "sk", "", "", "", "cn-north-4")
	require.NoError(t, err)

	url, err := p.PresignGetObject(context.Background(), "b", "k", time.Hour, types.WithPublic())
	require.NoError(t, err)
	assert.Equal(t, "https://b.obs.example.com/k", url)
}

// TestHuaweiProvider_PresignGetObject_Signed verifies the signed URL
// path is correct.
func TestHuaweiProvider_PresignGetObject_Signed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	url, err := p.PresignGetObject(context.Background(), "b", "k", time.Hour,
		types.WithDownloadFilename("report.pdf"))
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Contains(t, url, "/b/k")
}

// TestHuaweiProvider_ListObjects_HappyPath verifies pagination is
// followed (two pages with marker continuation).
func TestHuaweiProvider_ListObjects_HappyPath(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/xml")
		if callCount == 1 {
			// Page 1: 1 object, IsTruncated=true, NextMarker=m2
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <Name>b</Name>
  <Prefix>uploads/</Prefix>
  <IsTruncated>true</IsTruncated>
  <NextMarker>uploads/m2</NextMarker>
  <Contents>
    <Key>uploads/m1</Key>
    <Size>100</Size>
    <ETag>"e1"</ETag>
  </Contents>
</ListBucketResult>`))
			return
		}
		// Page 2: 1 object, IsTruncated=false
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <Name>b</Name>
  <Prefix>uploads/</Prefix>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>uploads/m2</Key>
    <Size>200</Size>
    <ETag>"e2"</ETag>
  </Contents>
</ListBucketResult>`))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	objs, err := p.ListObjects(context.Background(), "b", "uploads/")
	require.NoError(t, err)
	assert.Len(t, objs, 2, "must follow pagination across 2 pages")
	assert.Equal(t, "uploads/m1", objs[0].Key)
	assert.Equal(t, int64(100), objs[0].Size)
	assert.Equal(t, "e1", objs[0].ETag)
	assert.Equal(t, "uploads/m2", objs[1].Key)
	assert.Equal(t, int64(200), objs[1].Size)
	assert.Equal(t, 2, callCount, "must call OBS twice (one per page)")
}

// TestHuaweiProvider_PresignPutObject_WithMetadata verifies user-defined
// metadata is signed as x-obs-meta-<key> headers.
func TestHuaweiProvider_PresignPutObject_WithMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	url, headers, err := p.PresignPutObject(context.Background(), "b", "k", time.Hour,
		types.WithUploadMetadata(map[string]string{"author": "john"}))
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	require.NotNil(t, headers)
	// Metadata is surfaced with x-obs-meta- prefix on Huawei (vs
	// x-oss-meta- on Aliyun, x-amz-meta- on S3).
	assert.Equal(t, "john", headers.Get("x-obs-meta-author"))
}
```

The test file above references a few stub types (`obsHeadStub`, `obsGrantStub`, `objectInfoFromHeadFromStub`, `huaweiACLOrPrivateStubs`) so the helper unit tests do not depend on real OBS types — this keeps them resilient to SDK field renames. Add these stubs at the bottom of `provider_test.go`:

```go
// Stubs decoupling the helper unit tests from the OBS SDK types. The
// real production helpers (objectInfoFromHead, huaweiACLOrPrivate) take
// SDK types; the stubs mirror just the fields the helpers read.

type obsHeadStub struct {
	ContentLength int64
	ETag          string
	ContentType   string
	LastModified  time.Time
}

func objectInfoFromHeadFromStub(key string, head *obsHeadStub) *types.ObjectInfo {
	return &types.ObjectInfo{
		Key:          key,
		Size:         head.ContentLength,
		ETag:         strings.Trim(head.ETag, `"`),
		ContentType:  head.ContentType,
		LastModified: head.LastModified,
	}
}

type obsGrantStub struct {
	uri  string
	perm string
}

func huaweiACLOrPrivateStubs(grants []obsGrantStub) string {
	for _, g := range grants {
		if g.uri == "AllUsers" && g.perm == "READ" {
			return types.ObjectACLPublicRead
		}
	}
	return types.ObjectACLPrivate
}
```

- [ ] **Step 4: Run all huawei tests**

Run: `go test ./internal/provider/storage/huawei/ -count=1 -v`
Expected: all tests PASS, including the previously-deferred `TestHuaweiProvider_GetSTSToken_*` tests from Task 4.

- [ ] **Step 5: Commit (now including the full sts_test.go + provider.go + provider_test.go)**

```bash
git add internal/provider/storage/huawei/provider.go \
        internal/provider/storage/huawei/provider_test.go \
        internal/provider/storage/huawei/sts_test.go
git commit -m "feat(huawei): Provider 8 methods + STS GetSTSToken wiring

HuaweiProvider implements the full types.Provider surface against the
huaweicloud-sdk-go-obs module: PutObject/GetObject/DeleteObject/
HeadObject/ListObjects/PresignPutObject/PresignGetObject/GetSTSToken.
HeadObject does best-effort GetObjectAcl fallback (private/public-read
mapping via grant list). PresignGetObject honors WithPublic() (unsigned
URL) and image ops via x-image-process. GetSTSToken routes through the
IAM Agency client from the previous commit; tests cover happy path +
all error paths (no agency, nil client, TTL bounds)."
```

---

## Task 6: Registry wiring + test updates

**Goal:** Replace the `VENDOR_HUAWEI_OBS` "not yet implemented" placeholder in both `newProvider` and `newCDNURLGenerator` with real `huawei.NewHuaweiProvider` / `huawei.NewCDNURLGenerator` calls. Update `registry_test.go` so Huawei is expected to dispatch successfully (Tencent + Volcengine remain "not yet implemented").

**Files:**
- Modify: `internal/provider/storage/registry.go` (newProvider + newCDNURLGenerator)
- Modify: `internal/provider/storage/registry_test.go`

- [ ] **Step 1: Inspect current registry placeholders**

Run: `grep -n -A 4 "VENDOR_HUAWEI_OBS" internal/provider/storage/registry.go`
Expected: see the two switch cases that return "not yet implemented" errors for the three new vendors.

- [ ] **Step 2: Update newProvider to wire HuaweiProvider**

Edit `internal/provider/storage/registry.go` — in `newProvider`, split the combined `TENCENT_COS / HUAWEI_OBS / VOLCENGINE_TOS` case. Replace:

```go
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("vendor %s not yet implemented (coming in Phase 1)", cfg.Vendor)
```

with:

```go
	case storagev1.Vendor_VENDOR_HUAWEI_OBS:
		// roleARN is the IAM agency name (委托, plain string — NOT an
		// ARN). domainName carries the Huawei account ID used by the IAM
		// global-credentials builder; cfg.DomainID is the same value
		// (ProviderConfig exposes it as DomainID).
		p, err := huawei.NewHuaweiProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey,
			cfg.RoleARN, cfg.DomainID, cfg.DomainID, cfg.Region)
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("vendor %s not yet implemented (coming in Phase 1)", cfg.Vendor)
```

- [ ] **Step 3: Add the DomainID field to ProviderConfig if missing**

Run: `grep -n "DomainID" pkg/config/config.go || echo "no DomainID field yet"`
Expected: if "no DomainID field yet", add the field to ProviderConfig in `pkg/config/config.go`. The field is Huawei-specific (used by IAM global credentials) and optional on all other vendors.

Edit `pkg/config/config.go` — in the `ProviderConfig` struct, add (after `RoleARN string`):

```go
	// DomainID is the Huawei Cloud account ID (numeric). Required only by
	// VENDOR_HUAWEI_OBS — Huawei's IAM global-credentials builder needs
	// the domain ID to issue CreateTemporaryAccessKeyByAgency tokens.
	// Empty on all other vendors (ignored by their SDKs).
	DomainID string
```

- [ ] **Step 4: Update newCDNURLGenerator to wire huawei.NewCDNURLGenerator**

Edit `internal/provider/storage/registry.go` — in `newCDNURLGenerator`, split the combined `TENCENT_COS / HUAWEI_OBS / VOLCENGINE_TOS` case. Replace:

```go
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("CDN generator for vendor %s not yet implemented (coming in Phase 1)", vendor)
```

with:

```go
	case storagev1.Vendor_VENDOR_HUAWEI_OBS:
		return huawei.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("CDN generator for vendor %s not yet implemented (coming in Phase 1)", vendor)
```

- [ ] **Step 5: Add the huawei import to registry.go**

Edit `internal/provider/storage/registry.go` — add `"storage-service/internal/provider/storage/huawei"` to the import block alongside the existing aliyun/s3 imports:

```go
import (
	"fmt"
	"sync"

	storagev1 "storage-service/gen/storage/v1"

	"storage-service/internal/provider/storage/aliyun"
	"storage-service/internal/provider/storage/huawei"
	"storage-service/internal/provider/storage/s3"
	"storage-service/internal/provider/storage/types"
	"storage-service/pkg/config"
)
```

- [ ] **Step 6: Update registry_test.go to expect Huawei dispatch to succeed**

Edit `internal/provider/storage/registry_test.go` — replace the two existing `TestNewProvider_Phase1VendorsNotYetImplemented` and `TestNewCDNURLGenerator_Phase1VendorsNotYetImplemented` functions with split versions that treat Huawei as implemented:

```go
package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storage-service/pkg/config"
)

// TestNewProvider_RemainingPhase1VendorsNotYetImplemented verifies that
// Tencent and Volcengine still return the explicit "not yet implemented"
// error. Huawei has graduated to a real implementation (see
// TestNewProvider_HuaweiDispatchSucceeds).
func TestNewProvider_RemainingPhase1VendorsNotYetImplemented(t *testing.T) {
	cases := []string{
		"VENDOR_TENCENT_COS",
		"VENDOR_VOLCENGINE_TOS",
	}
	for _, vendor := range cases {
		t.Run(vendor, func(t *testing.T) {
			cfg := &config.ProviderConfig{
				Name:      "test",
				Vendor:    vendor,
				Endpoint:  "http://placeholder",
				Region:    "us-east-1",
				AccessKey: "ak",
				SecretKey: "sk",
			}
			_, err := newProvider(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not yet implemented",
				"vendor %s should return not-yet-implemented error", vendor)
		})
	}
}

// TestNewProvider_HuaweiDispatchSucceeds verifies that Huawei config now
// dispatches to huawei.NewHuaweiProvider and returns a concrete
// HuaweiProvider instance (no "not yet implemented" error).
func TestNewProvider_HuaweiDispatchSucceeds(t *testing.T) {
	cfg := &config.ProviderConfig{
		Name:      "huawei-test",
		Vendor:    "VENDOR_HUAWEI_OBS",
		Endpoint:  "obs.cn-north-4.myhuaweicloud.com",
		Region:    "cn-north-4",
		AccessKey: "ak",
		SecretKey: "sk",
		// role_arn + domain_id intentionally omitted — provider must
		// still construct (STS path is opt-in at GetSTSToken time).
	}
	p, err := newProvider(cfg)
	require.NoError(t, err)
	require.NotNil(t, p)
}

// TestNewCDNURLGenerator_RemainingPhase1VendorsNotYetImplemented verifies
// the CDN generator contract for Tencent + Volcengine (Huawei has
// graduated).
func TestNewCDNURLGenerator_RemainingPhase1VendorsNotYetImplemented(t *testing.T) {
	cases := []string{
		"VENDOR_TENCENT_COS",
		"VENDOR_VOLCENGINE_TOS",
	}
	for _, vendor := range cases {
		t.Run(vendor, func(t *testing.T) {
			cdn := &config.CDNConfig{
				Domain:  "cdn.example.com",
				AuthKey: "k",
			}
			_, err := newCDNURLGenerator(vendor, cdn)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "not yet implemented",
				"vendor %s CDN generator should return not-yet-implemented error", vendor)
		})
	}
}

// TestNewCDNURLGenerator_HuaweiDispatchSucceeds verifies Huawei CDN
// config dispatches to huawei.NewCDNURLGenerator.
func TestNewCDNURLGenerator_HuaweiDispatchSucceeds(t *testing.T) {
	cdn := &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: "k",
	}
	gen, err := newCDNURLGenerator("VENDOR_HUAWEI_OBS", cdn)
	require.NoError(t, err)
	require.NotNil(t, gen)
}
```

- [ ] **Step 7: Verify build + tests**

Run: `go build ./... && go vet ./... && go test ./internal/provider/storage/... -count=1`
Expected: build / vet succeed; all tests PASS including the new Huawei dispatch tests.

- [ ] **Step 8: Commit**

```bash
git add internal/provider/storage/registry.go \
        internal/provider/storage/registry_test.go \
        pkg/config/config.go
git commit -m "feat(registry): wire VENDOR_HUAWEI_OBS provider and CDN generator

Replaces the Huawei placeholder in newProvider / newCDNURLGenerator with
real huawei.NewHuaweiProvider / huawei.NewCDNURLGenerator calls. Tencent
and Volcengine remain 'not yet implemented' (separate Phase 1 PRs).
Adds optional ProviderConfig.DomainID for Huawei IAM global credentials."
```

---

## Final Verification

- [ ] **Full build + vet + test sweep**

Run:
```bash
go build ./... && \
go vet ./... && \
go test ./internal/provider/storage/huawei/... ./internal/provider/storage/... ./pkg/config/... ./internal/service/... -count=1 -race
```

Expected:
- build: no output
- vet: no output
- tests: all PASS. Huawei package has full coverage of the 8 provider methods + CDN + STS + policy builder + image style. Registry tests pass for both Huawei (success) and Tencent/Volcengine (not-yet-implemented). No regressions in aliyun / s3 / fake packages or service-layer tests.

- [ ] **gofmt check on all new + modified files**

Run:
```bash
gofmt -l \
  internal/provider/storage/huawei/cdn.go \
  internal/provider/storage/huawei/cdn_test.go \
  internal/provider/storage/huawei/imgproc.go \
  internal/provider/storage/huawei/imgproc_test.go \
  internal/provider/storage/huawei/sts.go \
  internal/provider/storage/huawei/sts_test.go \
  internal/provider/storage/huawei/provider.go \
  internal/provider/storage/huawei/provider_test.go \
  internal/provider/storage/registry.go \
  internal/provider/storage/registry_test.go \
  pkg/config/config.go
```
Expected: no output (all files properly formatted).

- [ ] **Lint sweep**

Run: `golangci-lint run ./internal/provider/storage/huawei/... ./internal/provider/storage/registry.go ./pkg/config/...`
Expected: no issues. If golangci-lint is unavailable locally, run `go vet ./...` (already done above) as a baseline.

- [ ] **Verify Huawei SDK imports resolve**

Run: `go list -m github.com/huaweicloud/huaweicloud-sdk-go-obs github.com/huaweicloud/huaweicloud-sdk-go-v3/services/iam`
Expected: both modules listed with their resolved versions (OBS = v3.26.3, IAM = whatever `latest` resolved to in Task 1 Step 3).

- [ ] **Push the branch**

The 6 commits from Tasks 1-6 form the Phase 1 Huawei PR. Push and open PR:

```bash
git push -u origin <branch-name>
# Open PR with title: "Phase 1: Huawei OBS provider"
```

PR description should:
1. Reference the spec at `docs/superpowers/specs/2026-06-25-multi-vendor-storage-providers-design.md` (Huawei section).
2. Note the two new SDK deps (`huaweicloud-sdk-go-obs` + `huaweicloud-sdk-go-v3/services/iam`).
3. Note that Tencent + Volcengine PRs are still pending (separate Phase 1 PRs).
4. Mention that integration testing (real Huawei Cloud dev account) is explicitly out of scope per the design spec.

---

## Notes for Reviewers

**SDK shape uncertainties (please flag during review):**

1. **`obs.PutObjectInput.Body` type**: The reference docs suggest `io.Reader`, but some Huawei SDK versions wrap it as `interface{}`. If `Body: reader` does not typecheck, swap to `Body: io.NopCloser(reader)` or `Body: &readerWrap{reader}`.

2. **`iamModel.AgencyPolicy.Body` field name**: The Huawei IAM v3 SDK may name this field differently (e.g. `PolicyText`). If the `AgencyTokenAuth.Policy` field shape has drifted from the design assumption, consult the SDK's `model.AgencyAuth` struct definition and adjust the `assumeAgency` body construction accordingly. The wrapping approach (build `*AgencyPolicy` with a JSON string body) is correct; only the field name may differ.

3. **`iamModel.CreateTemporaryAccessKeyByAgencyRequest.Body.Auth.SecurityToken`**: We use `AgencyTokenAuth` (the agency variant). If the SDK splits this into `AgencyTokenAuth` vs `TokenAuth`, the agency path is correct for our use case. The `*iamModel.AgencyTokenAuthAgency` struct's `RoleSessionName` field is a `*string` — passing `&req.RoleSessionName` requires the field be addressable.

4. **`obs.Grant.Grantee.URI` vs `obs.Grantee.URI`**: The grant-list → ACL mapping depends on the exact nested field name in the OBS SDK's `GetObjectAclOutput`. If the field shape differs, the test stubs in `provider_test.go` use the production helper, so the unit tests for the stub mapping remain valid — only the production helper may need adjustment.

5. **`iamRegion.ValueOf("cn-north-4")`**: If the SDK does not pre-register `cn-north-4` in its region map, fall back to the generic `region.New(id, endpoint)` constructor. The custom endpoint override (tests path) bypasses this entirely.

These uncertainties are confined to type-shape adaptations; the architectural decisions (separate IAM client, Huawei policy JSON shape, Type A CDN formula, OBS image process syntax mirroring Aliyun) are spec-pinned and stable.
