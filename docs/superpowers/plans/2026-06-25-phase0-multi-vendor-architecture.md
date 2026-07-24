# Phase 0: Multi-Vendor Architecture Foundation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lay the architectural groundwork for adding Tencent COS, Huawei OBS, and Volcengine TOS providers in later Phase 1 PRs. This PR is independently mergeable — existing aliyun/s3 deployments remain functional, new vendors return "unsupported vendor" until Phase 1 fills them in.

**Architecture:**
- Extend proto `Vendor` enum with three new values (`VENDOR_TENCENT_COS=4`, `VENDOR_HUAWEI_OBS=5`, `VENDOR_VOLCENGINE_TOS=6`)
- Delete dead `CDNConfig.AuthType` field (runtime never reads it; vendor is the single source of truth for generator selection)
- Add `default` cases to `newProvider` and `newCDNURLGenerator` returning explicit "unsupported vendor" errors for the three new values, so Phase 1 PRs just fill in the case bodies
- Document the new vendors in `config.example.yaml` (commented examples only — they'll fail validation until Phase 1)

**Tech Stack:** Go 1.22+, buf/protobuf, viper config, testify.

**Spec:** `docs/superpowers/specs/2026-06-25-multi-vendor-storage-providers-design.md`

---

## File Map

| File | Responsibility | Created/Modified |
|------|----------------|------------------|
| `api/proto/storage/v1/storage.proto` | `Vendor` enum extension | Modified (Task 1) |
| `gen/storage/v1/storage.pb.go` | Auto-generated enum code | Regenerated (Task 1) |
| `pkg/config/config.go` | Remove `AuthType` field; `validateBucketCDN` switches to vendor-based `KeyPairID` check | Modified (Task 2) |
| `pkg/config/config_test.go` | Drop `AuthType` test fixtures; add vendor→KeyPairID fixture | Modified (Task 2) |
| `internal/provider/storage/aliyun/cdn_test.go` | Drop `AuthType: "aliyun-type-a"` from fixtures | Modified (Task 2) |
| `internal/provider/storage/s3/cdn_test.go` | Drop `AuthType: "cloudfront"` from fixtures | Modified (Task 2) |
| `internal/provider/storage/registry.go` | Add explicit cases for new vendor enum values returning "unsupported" errors | Modified (Task 3) |
| `internal/provider/storage/registry_test.go` | New file: verify new vendors return unsupported error | Created (Task 3) |
| `config.example.yaml` | Remove `auth_type` lines; add commented examples for 3 new vendors | Modified (Task 4) |

---

## Task 1: Extend proto Vendor enum

**Goal:** Add the three new vendor enum values so the rest of the codebase can reference them. `buf generate` regenerates Go bindings.

**Files:**
- Modify: `api/proto/storage/v1/storage.proto:24-28`

- [ ] **Step 1: Read current Vendor enum**

Run: `grep -n -A 6 "^enum Vendor" api/proto/storage/v1/storage.proto`
Expected output:
```
24:enum Vendor {
25:  VENDOR_UNSPECIFIED = 0;
26:  VENDOR_ALIYUN_OSS = 1;
27:  VENDOR_AWS_S3 = 2;
28:  VENDOR_S3_COMPATIBLE = 3;
29:}
```

- [ ] **Step 2: Add three new enum values**

Edit `api/proto/storage/v1/storage.proto` — replace the enum block:

```protobuf
enum Vendor {
  VENDOR_UNSPECIFIED = 0;
  VENDOR_ALIYUN_OSS = 1;
  VENDOR_AWS_S3 = 2;
  VENDOR_S3_COMPATIBLE = 3;
  VENDOR_TENCENT_COS = 4;
  VENDOR_HUAWEI_OBS = 5;
  VENDOR_VOLCENGINE_TOS = 6;
}
```

Note: field numbers 1/2/3 are immutable (proto3 wire compat). 4/5/6 are new.

- [ ] **Step 3: Regenerate Go bindings**

Run: `buf generate`
Expected: command exits 0 with no output.

- [ ] **Step 4: Verify the new enum values are in generated code**

Run: `grep -n "VENDOR_TENCENT_COS\|VENDOR_HUAWEI_OBS\|VENDOR_VOLCENGINE_TOS" gen/storage/v1/storage.pb.go`
Expected output:
```
  VENDOR_TENCENT_COS
  VENDOR_HUAWEI_OBS
  VENDOR_VOLCENGINE_TOS
```
(may also show entries in `Vendor_name` / `Vendor_value` maps)

- [ ] **Step 5: Verify build still passes**

Run: `go build ./...`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add api/proto/storage/v1/storage.proto gen/storage/v1/storage.pb.go
git commit -m "feat(proto): add VENDOR_TENCENT_COS / VENDOR_HUAWEI_OBS / VENDOR_VOLCENGINE_TOS"
```

---

## Task 2: Delete CDNConfig.AuthType field

**Goal:** Remove the dead `AuthType` field. `validateBucketCDN` switches to a vendor-based `KeyPairID` check (cloudfront path requires it; others must leave it empty). Update all test fixtures and example yaml.

**Files:**
- Modify: `pkg/config/config.go:193-207` (struct), `pkg/config/config.go:351-385` (validateBucketCDN)
- Modify: `pkg/config/config_test.go` (multiple fixtures)
- Modify: `internal/provider/storage/aliyun/cdn_test.go:23-28`
- Modify: `internal/provider/storage/s3/cdn_test.go:42-52`

- [ ] **Step 1: Read current CDNConfig struct + validateBucketCDN**

Run: `grep -n -A 20 "^type CDNConfig\|^func validateBucketCDN" pkg/config/config.go`
Expected: see struct with `AuthType` field at line ~205 and validateBucketCDN at line ~358.

- [ ] **Step 2: Remove AuthType field from struct**

Edit `pkg/config/config.go` — replace the `CDNConfig` struct definition:

```go
// CDNConfig configures CDN signing for a single bucket. nil on BucketConfig
// means CDN is disabled for that bucket (GenerateCDNURL returns
// ErrCDNNotConfigured).
//
// CDN in the cloud-vendor sense is per-bucket: Aliyun CDN's origin is one
// specific OSS bucket; CloudFront's distribution origin is one specific S3
// bucket. Keeping the config at bucket level (rather than provider level)
// matches reality and lets two buckets under the same provider use different
// CDN domains.
//
// Generator selection is by parent ProviderConfig.Vendor (not by an explicit
// auth-type field). KeyPairID is only meaningful for the cloudfront path
// (VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE); validateBucketCDN enforces this.
type CDNConfig struct {
	// Domain is a bare hostname (e.g. cdn.example.com). No scheme, no path,
	// no trailing slash — validateCDNDomain enforces this. The URL scheme is
	// always https (see types.SchemeHTTPS); http CDN distribution is not
	// supported.
	Domain string
	// AuthKey is the signing key. Semantics depend on vendor:
	//   - Aliyun/Huawei/Volcengine: literal primary key from CDN console
	//     (used in MD5 auth_key / equivalent).
	//   - AWS S3 / S3-compatible (cloudfront): file path to a PEM private key.
	AuthKey string
	// KeyPairID is the CloudFront key pair ID (required only for the
	// cloudfront path, i.e. VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE).
	// validateBucketCDN rejects non-empty KeyPairID on other vendors and
	// empty KeyPairID on cloudfront vendors.
	KeyPairID string
}
```

- [ ] **Step 3: Replace validateBucketCDN with vendor-based KeyPairID check**

Edit `pkg/config/config.go` — replace the body of `validateBucketCDN`:

```go
// validateBucketCDN checks a single bucket's CDNConfig. vendor is the parent
// provider's vendor string (e.g. VENDOR_ALIYUN_OSS).
//
// KeyPairID requirement is vendor-driven (formerly driven by AuthType):
//   - VENDOR_AWS_S3 / VENDOR_S3_COMPATIBLE (cloudfront path): KeyPairID required
//   - All other vendors: KeyPairID must be empty (the field is meaningless)
//
// Path prefix reflects the bucket-level config location:
// storage.providers[i].buckets[j].cdn.
func validateBucketCDN(i, j int, cdn *CDNConfig, vendor string) error {
	if cdn.Domain == "" {
		return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.domain is required when cdn is set", i, j)
	}
	if err := validateCDNDomain(cdn.Domain); err != nil {
		return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.domain: %w", i, j, err)
	}
	if cdn.AuthKey == "" {
		return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.auth_key is required when cdn is set", i, j)
	}
	switch vendor {
	case "VENDOR_AWS_S3", "VENDOR_S3_COMPATIBLE":
		if cdn.KeyPairID == "" {
			return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.key_pair_id is required for vendor %q (cloudfront signing)", i, j, vendor)
		}
	default:
		if cdn.KeyPairID != "" {
			return fmt.Errorf("storage.providers[%d].buckets[%d].cdn.key_pair_id is not used by vendor %q (only cloudfront path needs it)", i, j, vendor)
		}
	}
	return nil
}
```

- [ ] **Step 4: Update validConfigWithCDN helper in config_test.go**

Edit `pkg/config/config_test.go` — find `validConfigWithCDN` (around line 410) and replace:

```go
// validConfigWithCDN returns validConfig() with the given CDN attached to
// the first provider's first bucket. Vendor is switched to match the CDN's
// needs:
//   - When KeyPairID is non-empty (cloudfront path): vendor → VENDOR_AWS_S3
//   - Otherwise: vendor stays as the default VENDOR_S3_COMPATIBLE only if
//     the test doesn't pre-set one. For Aliyun-style tests (no KeyPairID),
//     caller must set VENDOR_ALIYUN_OSS explicitly before calling.
func validConfigWithCDN(t *testing.T, cdn *CDNConfig) *Config {
	t.Helper()
	cfg := validConfig(t)
	cfg.Storage.Providers[0].Buckets[0].CDN = cdn
	if cdn.KeyPairID != "" {
		// cloudfront path needs AWS / S3-compatible vendor
		cfg.Storage.Providers[0].Vendor = "VENDOR_AWS_S3"
	}
	return cfg
}
```

- [ ] **Step 5: Audit remaining AuthType references in config_test.go**

Run: `grep -n "AuthType\|auth_type" pkg/config/config_test.go`
Expected output: lines mentioning AuthType in tests like `TestCDNConfig_Validate_BadAuthType`, `TestCDNConfig_Validate_ValidAliyun`, etc.

- [ ] **Step 6: Delete the BadAuthType test (no longer applicable)**

Edit `pkg/config/config_test.go` — delete the entire `TestCDNConfig_Validate_BadAuthType` function:

```go
// (DELETE THIS ENTIRE FUNCTION)
// TestCDNConfig_Validate_BadAuthType verifies only known auth types pass.
func TestCDNConfig_Validate_BadAuthType(t *testing.T) {
	...
}
```

- [ ] **Step 7: Strip AuthType from remaining test fixtures**

For each remaining test function in `pkg/config/config_test.go` that constructs a `CDNConfig{...}`, remove the `AuthType: ...` line. Examples:

- `TestCDNConfig_Validate_DomainRequired`: remove `AuthType: "aliyun-type-a"` line; ensure vendor is VENDOR_ALIYUN_OSS (set explicitly)
- `TestCDNConfig_Validate_BadDomainFormat`: same
- `TestCDNConfig_Validate_MissingAuthKey`: same
- `TestCDNConfig_Validate_CloudFrontRequiresKeyPairID`: remove `AuthType: "cloudfront"` line; KeyPairID-driven vendor switching via validConfigWithCDN handles it
- `TestCDNConfig_Validate_ValidAliyun`: remove `AuthType: "aliyun-type-a"`; ensure vendor is VENDOR_ALIYUN_OSS
- `TestCDNConfig_Validate_ValidCloudFront`: remove `AuthType: "cloudfront"`; KeyPairID-driven vendor switching handles it

For tests that need VENDOR_ALIYUN_OSS (where KeyPairID is empty), explicitly set vendor in the test body:

```go
cfg := validConfigWithCDN(t, &CDNConfig{Domain: "cdn.example.com", AuthKey: "k"})
cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"  // explicit, since KeyPairID is empty
```

- [ ] **Step 8: Add a new test for KeyPairID-vs-vendor enforcement**

Append to `pkg/config/config_test.go`:

```go
// TestCDNConfig_Validate_KeyPairIDVendorMismatch verifies KeyPairID is
// rejected on non-cloudfront vendors and required on cloudfront vendors.
// Replaces the deleted AuthType consistency check.
func TestCDNConfig_Validate_KeyPairIDVendorMismatch(t *testing.T) {
	t.Run("KeyPairID on Aliyun rejected", func(t *testing.T) {
		cfg := validConfigWithCDN(t, &CDNConfig{
			Domain:    "cdn.example.com",
			AuthKey:   "k",
			KeyPairID: "APKAJXXXX",
		})
		cfg.Storage.Providers[0].Vendor = "VENDOR_ALIYUN_OSS"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key_pair_id is not used by vendor")
	})

	t.Run("KeyPairID missing on cloudfront rejected", func(t *testing.T) {
		cfg := validConfigWithCDN(t, &CDNConfig{
			Domain:  "cdn.example.com",
			AuthKey: "/path/to/key.pem",
			// KeyPairID intentionally empty
		})
		// Vendor defaults to VENDOR_S3_COMPATIBLE in validConfig
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key_pair_id is required for vendor")
	})
}
```

- [ ] **Step 9: Run config tests**

Run: `go test ./pkg/config/... -count=1 -v -run 'CDN'`
Expected: all CDN tests PASS. If any fail with "unknown field AuthType" or similar, fix the fixture (Step 7 was incomplete).

- [ ] **Step 10: Strip AuthType from aliyun CDN test fixture**

Edit `internal/provider/storage/aliyun/cdn_test.go` — find `aliyunCDNConfig` function (~line 23):

```go
// Before:
func aliyunCDNConfig(authKey string) *config.CDNConfig {
	return &config.CDNConfig{
		Domain:   "cdn.example.com",
		AuthType: "aliyun-type-a",
		AuthKey:  authKey,
	}
}

// After:
func aliyunCDNConfig(authKey string) *config.CDNConfig {
	return &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: authKey,
	}
}
```

- [ ] **Step 11: Strip AuthType from s3 CDN test fixtures**

Edit `internal/provider/storage/s3/cdn_test.go` — find `cloudfrontCDNConfig` function (~line 47):

```go
// Before:
func cloudfrontCDNConfig(t *testing.T) *config.CDNConfig {
	t.Helper()
	return &config.CDNConfig{
		Domain:    "cdn.example.com",
		AuthType:  "cloudfront",
		AuthKey:   writeTestPEM(t),
		KeyPairID: "K2A1B2C3D4E5F6",
	}
}

// After:
func cloudfrontCDNConfig(t *testing.T) *config.CDNConfig {
	t.Helper()
	return &config.CDNConfig{
		Domain:    "cdn.example.com",
		AuthKey:   writeTestPEM(t),
		KeyPairID: "K2A1B2C3D4E5F6",
	}
}
```

- [ ] **Step 12: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 13: Run all CDN-related tests**

Run: `go test ./pkg/config/... ./internal/provider/storage/aliyun/... ./internal/provider/storage/s3/... ./internal/service/... -count=1`
Expected: all PASS except the pre-existing `TestS3Provider_PresignGetObject/PutObject` failures (testcontainer endpoint issue, unrelated to this change).

- [ ] **Step 14: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go \
        internal/provider/storage/aliyun/cdn_test.go \
        internal/provider/storage/s3/cdn_test.go
git commit -m "refactor(config): drop dead CDNConfig.AuthType field

AuthType was tautological — vendor already selects the generator. Field
removed; validateBucketCDN now gates KeyPairID purely on vendor (required
for cloudfront path, rejected elsewhere). Test fixtures cleaned up."
```

---

## Task 2.5: Expand ProviderConfig.RoleARN doc comment for per-vendor semantics

**Goal:** The RoleARN field's format and required-ness vary by vendor. Update the doc comment so operators know what to fill in. Pure documentation change — no behavior change.

**Files:**
- Modify: `pkg/config/config.go` (RoleARN field comment in ProviderConfig struct)

- [ ] **Step 1: Read current RoleARN doc comment**

Run: `grep -n -B 1 -A 8 "RoleARN string" pkg/config/config.go`
Expected: see current comment about IAM/RAM role ARN with Aliyun + AWS + MinIO examples only.

- [ ] **Step 2: Replace the RoleARN doc comment**

Edit `pkg/config/config.go` — find the RoleARN field in the `ProviderConfig` struct and replace its doc comment:

```go
	// RoleARN is the IAM/RAM role reference for STS AssumeRole. Format and
	// required-ness vary by vendor:
	//
	//   - VENDOR_ALIYUN_OSS:     "acs:ram::<account-id>:role/<role-name>"
	//                            (required for STS; empty = STS unavailable)
	//   - VENDOR_AWS_S3:         "arn:aws:iam::<account-id>:role/<role-name>"
	//                            (required for STS; empty = STS unavailable)
	//   - VENDOR_S3_COMPATIBLE:  any non-empty identifier — MinIO doesn't
	//                            validate the format (optional)
	//   - VENDOR_TENCENT_COS:    UNUSED — Tencent CAM STS issues temp
	//                            credentials directly from policy without
	//                            a role. Leave empty.
	//   - VENDOR_HUAWEI_OBS:     agency name (委托名, plain string, NOT an
	//                            ARN) from IAM console. Required for STS.
	//   - VENDOR_VOLCENGINE_TOS: "trn:iam::<account-id>:role/<role-name>".
	//                            Required for STS.
	//
	// Format validation is per-vendor at provider construction time
	// (NewXxxProvider returns error on malformed non-empty RoleARN). Empty
	// = STS unavailable for this provider; clients must use GenerateUploadURL.
	RoleARN string
```

- [ ] **Step 3: Build + vet**

Run: `go build ./... && go vet ./...`
Expected: no output (comment-only change can't break build).

- [ ] **Step 4: Commit**

```bash
git add pkg/config/config.go
git commit -m "docs(config): expand ProviderConfig.RoleARN comment for 6 vendors

Documents per-vendor RoleARN semantics: format (Aliyun ARN / AWS ARN /
Huawei agency name / Volcengine TRN) and required-ness (Tencent doesn't
use it). Helps operators fill in the right value when adding Phase 1
vendors."
```

---

## Task 3: Add unsupported-vendor cases to Registry

**Goal:** When Phase 1 PRs land, they'll fill in the case bodies. For now, an explicit error message guides operators who configure a new vendor before its provider ships.

**Files:**
- Modify: `internal/provider/storage/registry.go:201-231` (newProvider), `internal/provider/storage/registry.go:233-256` (newCDNURLGenerator)
- Create: `internal/provider/storage/registry_test.go`

- [ ] **Step 1: Read current newProvider and newCDNURLGenerator**

Run: `grep -n -A 30 "^func newProvider\|^func newCDNURLGenerator" internal/provider/storage/registry.go`
Expected: see switch on `storagev1.Vendor(v)` with cases for ALIYUN_OSS / AWS_S3 / S3_COMPATIBLE and a default returning "unsupported vendor".

- [ ] **Step 2: Add explicit cases for the three new vendors in newProvider**

Edit `internal/provider/storage/registry.go` — in `newProvider`, before the `default:` case, add:

```go
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("vendor %s not yet implemented (coming in Phase 1)", cfg.Vendor)
```

The full switch should now look like:

```go
	switch storagev1.Vendor(v) {
	case storagev1.Vendor_VENDOR_AWS_S3, storagev1.Vendor_VENDOR_S3_COMPATIBLE:
		p, err := s3.NewS3Provider(cfg.Endpoint, cfg.Region, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN)
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_ALIYUN_OSS:
		p, err := aliyun.NewAliyunProvider(cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.RoleARN, cfg.Region)
		if err != nil {
			return nil, err
		}
		return p, nil
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("vendor %s not yet implemented (coming in Phase 1)", cfg.Vendor)
	default:
		return nil, fmt.Errorf("unsupported vendor: %s", cfg.Vendor)
	}
```

- [ ] **Step 3: Add explicit cases in newCDNURLGenerator**

Edit `internal/provider/storage/registry.go` — in `newCDNURLGenerator`, before the `default:` case, add the same three vendors:

```go
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("CDN generator for vendor %s not yet implemented (coming in Phase 1)", vendor)
```

The full switch:

```go
	switch storagev1.Vendor(v) {
	case storagev1.Vendor_VENDOR_AWS_S3, storagev1.Vendor_VENDOR_S3_COMPATIBLE:
		return s3.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_ALIYUN_OSS:
		return aliyun.NewCDNURLGenerator(cdn), nil
	case storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return nil, fmt.Errorf("CDN generator for vendor %s not yet implemented (coming in Phase 1)", vendor)
	default:
		return nil, fmt.Errorf("CDN not supported for vendor %s", vendor)
	}
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: no output.

- [ ] **Step 5: Create registry_test.go with unsupported-vendor coverage**

Create `internal/provider/storage/registry_test.go`:

```go
package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"storage-service/pkg/config"
)

// TestNewProvider_UnsupportedVendor verifies that the three new vendor enum
// values (Tencent/Huawei/Volcengine) return an explicit "not yet implemented"
// error rather than silently falling through to "unsupported vendor".
// Phase 1 PRs will replace the error with real provider construction.
func TestNewProvider_Phase1VendorsNotYetImplemented(t *testing.T) {
	cases := []string{
		"VENDOR_TENCENT_COS",
		"VENDOR_HUAWEI_OBS",
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

// TestNewCDNURLGenerator_Phase1VendorsNotYetImplemented verifies the same
// contract for CDN generator selection.
func TestNewCDNURLGenerator_Phase1VendorsNotYetImplemented(t *testing.T) {
	cases := []string{
		"VENDOR_TENCENT_COS",
		"VENDOR_HUAWEI_OBS",
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
```

- [ ] **Step 6: Run the new tests**

Run: `go test ./internal/provider/storage/ -count=1 -v -run 'Phase1'`
Expected: 6 sub-tests PASS (3 vendor × 2 functions).

- [ ] **Step 7: Commit**

```bash
git add internal/provider/storage/registry.go internal/provider/storage/registry_test.go
git commit -m "feat(registry): explicit not-yet-implemented cases for Phase 1 vendors

Tencent/Huawei/Volcengine enum values are now valid proto, but their
providers ship in Phase 1 PRs. Until then, configuration with these
vendors fails fast with a clear 'not yet implemented (coming in Phase 1)'
message instead of the generic 'unsupported vendor' fallthrough."
```

---

## Task 4: Update config.example.yaml

**Goal:** Remove `auth_type` lines from existing examples (field is gone). Add commented examples for the three new vendors so operators know what Phase 1 will enable.

**Files:**
- Modify: `config.example.yaml:94-133` (existing Aliyun/S3 CDN examples + add new vendor blocks)

- [ ] **Step 1: Read existing CDN example block**

Run: `grep -n "auth_type\|cdn:\|VENDOR_" config.example.yaml`
Expected: see `auth_type:` lines in the commented Aliyun and S3 CDN examples.

- [ ] **Step 2: Strip auth_type lines from existing examples**

Edit `config.example.yaml` — in the Aliyun CDN example block (~line 103), remove the `auth_type: "aliyun-type-a"` line:

```yaml
    # Example Aliyun OSS provider with CDN (Type A signed URL) acceleration.
    # CDN config lives under the bucket — same provider can host multiple
    # buckets with different CDN domains. Uncomment and fill in.
    # - name: aliyun-prod-cdn
    #   vendor: VENDOR_ALIYUN_OSS
    #   region: cn-hangzhou
    #   endpoint: oss-cn-hangzhou.aliyuncs.com
    #   access_key: ${ALIYUN_AK}
    #   secret_key: ${ALIYUN_SK}
    #   role_arn: "acs:ram::1234567890:role/storage-uploader"
    #   buckets:
    #     - name: photos
    #       key_prefix: "uploads/"
    #       acl: private
    #       cdn:
    #         # CDN URL signing — required for GenerateCDNURL RPC. The same key
    #         # must be configured in the Aliyun CDN console (Domain → Access
    #         # Control → URL Authentication → Type A → Primary Key).
    #         domain: cdn.example.com
    #         auth_key: ${ALIYUN_CDN_AUTH_KEY}
```

(Notice: no `auth_type` line — the generator is selected by the parent provider's vendor.)

Apply the same removal to the S3/CloudFront example block (~line 115): drop the `auth_type: "cloudfront"` line.

- [ ] **Step 3: Append commented examples for the three new vendors**

Edit `config.example.yaml` — after the existing CloudFront example block, before the `third_party:` section, append:

```yaml
    # ------------------------------------------------------------------
    # Phase 1 vendors — not yet implemented. Configurations below will
    # fail validation with "vendor VENDOR_X not yet implemented (coming
    # in Phase 1)" until the corresponding provider PR lands.
    # ------------------------------------------------------------------

    # Tencent Cloud COS (Type A signed URL). Bucket name MUST include the
    # APPID suffix (e.g. mybucket-1250000000). STS uses CAM STS — RoleARN
    # is NOT used; leave it empty.
    # - name: tencent-prod
    #   vendor: VENDOR_TENCENT_COS
    #   endpoint: cos.ap-guangzhou.myqcloud.com
    #   region: ap-guangzhou
    #   access_key: ${TENCENT_SECRET_ID}
    #   secret_key: ${TENCENT_SECRET_KEY}
    #   # role_arn intentionally omitted — Tencent CAM STS doesn't use it
    #   buckets:
    #     - name: mybucket-1250000000
    #       key_prefix: "uploads/"
    #       acl: private
    #       cdn:
    #         domain: cdn.example.com
    #         auth_key: ${TENCENT_CDN_AUTH_KEY}

    # Huawei Cloud OBS (Type A signed URL). STS uses IAM Agency —
    # role_arn is the Agency name (NOT an ARN).
    # - name: huawei-prod
    #   vendor: VENDOR_HUAWEI_OBS
    #   endpoint: obs.cn-north-4.myhuaweicloud.com
    #   region: cn-north-4
    #   access_key: ${HUAWEI_AK}
    #   secret_key: ${HUAWEI_SK}
    #   role_arn: "storage-uploader-agency"  # Agency name from IAM console
    #   buckets:
    #     - name: mybucket
    #       key_prefix: "uploads/"
    #       acl: private
    #       cdn:
    #         domain: cdn.example.com
    #         auth_key: ${HUAWEI_CDN_AUTH_KEY}

    # Volcengine TOS (Type A signed URL). STS uses IAM AssumeRole with TRN
    # format. Bucket ACL is enforced via TOS PutObjectACL.
    # - name: volcengine-prod
    #   vendor: VENDOR_VOLCENGINE_TOS
    #   endpoint: tos-cn-beijing.volces.com
    #   region: cn-beijing
    #   access_key: ${VOLC_AK}
    #   secret_key: ${VOLC_SK}
    #   role_arn: "trn:iam::1000000000:role/storage-uploader"
    #   buckets:
    #     - name: mybucket
    #       key_prefix: "uploads/"
    #       acl: private
    #       cdn:
    #         domain: cdn.example.com
    #         auth_key: ${VOLC_CDN_AUTH_KEY}
```

- [ ] **Step 4: Verify the yaml is still syntactically valid (no parsing of commented blocks)**

Run: `go test ./pkg/config/... -count=1 -run TestConfigDefault`
Expected: PASS (this test loads config.example.yaml via viper). Even though new entries are commented out, viper should parse the file without error.

If no such test exists, run: `go test ./pkg/config/... -count=1`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add config.example.yaml
git commit -m "docs(config): drop auth_type from examples; add Phase 1 vendor placeholders

auth_type field is removed from CDNConfig (vendor drives generator
selection). Adds commented examples for Tencent COS, Huawei OBS, and
Volcengine TOS — they will fail validation with a clear 'not yet
implemented' error until Phase 1 provider PRs land."
```

---

## Final Verification

- [ ] **Full build + vet + test sweep**

Run:
```bash
go build ./... && \
go vet ./... && \
go test ./pkg/config/... ./internal/provider/storage/... ./internal/service/... -count=1
```

Expected:
- build: no output
- vet: no output
- tests: all PASS except pre-existing `TestS3Provider_PresignGetObject` / `TestS3Provider_PresignPutObject` (testcontainer endpoint issue, unrelated)

- [ ] **gofmt check**

Run: `gofmt -l internal/provider/storage/registry.go internal/provider/storage/registry_test.go pkg/config/config.go pkg/config/config_test.go internal/provider/storage/aliyun/cdn_test.go internal/provider/storage/s3/cdn_test.go`
Expected: no output (all files properly formatted).

- [ ] **Push the branch**

The 4 commits from Tasks 1-4 form the Phase 0 PR. Push and open PR:

```bash
git push -u origin <branch-name>
# Open PR with title: "Phase 0: multi-vendor architecture foundation"
```

PR description should reference the spec at `docs/superpowers/specs/2026-06-25-multi-vendor-storage-providers-design.md` and note that Phase 1 PRs (Tencent/Huawei/Volcengine) will follow in parallel after this merges.

---

## What's Next (Phase 1)

After this plan completes and Phase 0 merges, three independent plans follow:

- **Plan 2: PR-tencent** — first vendor provider, sets the structural template
- **Plan 3: PR-huawei** — mirrors Tencent structure with Huawei-specific STS/IAM PolicyBuilder
- **Plan 4: PR-volcengine** — mirrors Tencent structure with native TOS SDK

Each Phase 1 plan removes one case from the "not yet implemented" block in `registry.go` (Task 3 of this plan) and replaces it with real provider/generator construction.
