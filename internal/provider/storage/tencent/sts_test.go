package tencent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// errFakeSTS is the sentinel error used by TestTencentProvider_GetSTSToken_STSErrorPropagates
// to verify errors.Is unwrap chain through GetSTSToken.
var errFakeSTS = errors.New("fake sts failure")

// TestBuildTencentPolicy_NoExtensions verifies empty AllowedExtensions
// yields a single Resource wildcard covering the entire prefix. Empty
// region/appID preserve legacy wildcard semantics.
func TestBuildTencentPolicy_NoExtensions(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:    "photos-1250000000",
		KeyPrefix: "uploads/",
	}, "", "")
	require.NoError(t, err)

	assert.Equal(t, "2.0", policy["version"])
	stmts := policy["statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	assert.Equal(t, "allow", stmts[0]["effect"])
	assert.Equal(t, []string{"name/cos:PostObject", "name/cos:PutObject"}, stmts[0]["action"])
	assert.Equal(t, []string{"qcs::cos:*:uid/*:photos-1250000000/uploads/*"}, stmts[0]["resource"])
	// No hardening flags set → condition must be absent (CAM rejects an empty
	// Condition block).
	_, hasCond := stmts[0]["condition"]
	assert.False(t, hasCond, "condition should be absent when no hardening flags set")
}

// TestBuildTencentPolicy_WithExtensions verifies each extension becomes a
// separate Resource wildcard entry.
func TestBuildTencentPolicy_WithExtensions(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:            "photos-1250000000",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
	}, "", "")
	require.NoError(t, err)

	stmts := policy["statement"].([]map[string]any)
	resources := stmts[0]["resource"].([]string)
	assert.Equal(t, []string{
		"qcs::cos:*:uid/*:photos-1250000000/uploads/*.jpg",
		"qcs::cos:*:uid/*:photos-1250000000/uploads/*.png",
	}, resources)
}

// TestBuildTencentPolicy_BadExtensionFormat verifies extensions missing the
// '.' prefix are rejected.
func TestBuildTencentPolicy_BadExtensionFormat(t *testing.T) {
	_, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:            "photos-1250000000",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{"jpg"},
	}, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with '.'")
}

// TestBuildTencentPolicy_CustomActions verifies AllowedActions override
// default. Note Tencent requires "name/cos:" prefix on action strings.
func TestBuildTencentPolicy_CustomActions(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:         "photos-1250000000",
		KeyPrefix:      "uploads/",
		AllowedActions: []string{"name/cos:PutObject", "name/cos:GetObject"},
	}, "", "")
	require.NoError(t, err)
	stmts := policy["statement"].([]map[string]any)
	assert.Equal(t, []string{"name/cos:PutObject", "name/cos:GetObject"}, stmts[0]["action"])
}

// TestBuildTencentPolicy_KeyPrefixTrailingSlashStripped verifies prefix
// normalization (no double slash).
func TestBuildTencentPolicy_KeyPrefixTrailingSlashStripped(t *testing.T) {
	for _, prefix := range []string{"uploads/", "uploads"} {
		policy, err := buildTencentPolicy(&types.STSPolicy{
			Bucket:    "photos-1250000000",
			KeyPrefix: prefix,
		}, "", "")
		require.NoError(t, err)
		stmts := policy["statement"].([]map[string]any)
		resources := stmts[0]["resource"].([]string)
		assert.Equal(t, []string{"qcs::cos:*:uid/*:photos-1250000000/uploads/*"}, resources,
			"prefix %q should normalize", prefix)
	}
}

// TestBuildTencentPolicy_EmptyOrSlashKeyPrefix verifies that an empty or
// "/" KeyPrefix produces a single-slash resource base. Without this guard
// the format string yields "qcs::cos:*:uid/*:bucket//*" (double slash) which
// Tencent CAM matches literally and silently rejects at PUT time.
func TestBuildTencentPolicy_EmptyOrSlashKeyPrefix(t *testing.T) {
	for _, prefix := range []string{"", "/", "//"} {
		policy, err := buildTencentPolicy(&types.STSPolicy{
			Bucket:    "photos-1250000000",
			KeyPrefix: prefix,
		}, "", "")
		require.NoError(t, err)
		stmts := policy["statement"].([]map[string]any)
		resources := stmts[0]["resource"].([]string)
		assert.Equal(t, []string{"qcs::cos:*:uid/*:photos-1250000000/*"}, resources,
			"prefix %q should normalize to bucket-only resource", prefix)
	}
}

// TestBuildTencentPolicy_RegionAndAppIDScope verifies that non-empty region
// and appID tighten the Resource ARN from wildcard to scoped values. This is
// the core credential-hardening guarantee: a leaked STS token cannot be
// replayed against same-named buckets in other regions or accounts.
func TestBuildTencentPolicy_RegionAndAppIDScope(t *testing.T) {
	cases := []struct {
		name     string
		region   string
		appID    string
		wantBase string
	}{
		{"both empty -> wildcard", "", "", "qcs::cos:*:uid/*:photos-1250000000/uploads/*"},
		{"region only", "ap-guangzhou", "", "qcs::cos:ap-guangzhou:uid/*:photos-1250000000/uploads/*"},
		{"appid only", "", "1250000000", "qcs::cos:*:uid/1250000000:photos-1250000000/uploads/*"},
		{"both scoped", "ap-guangzhou", "1250000000", "qcs::cos:ap-guangzhou:uid/1250000000:photos-1250000000/uploads/*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := buildTencentPolicy(&types.STSPolicy{
				Bucket:    "photos-1250000000",
				KeyPrefix: "uploads/",
			}, tc.region, tc.appID)
			require.NoError(t, err)
			stmts := policy["statement"].([]map[string]any)
			resources := stmts[0]["resource"].([]string)
			assert.Equal(t, []string{tc.wantBase}, resources)
		})
	}
}

// TestBuildTencentPolicy_EnforceHTTPS verifies the Bool Condition that blocks
// plaintext HTTP uploads at COS.
func TestBuildTencentPolicy_EnforceHTTPS(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:       "photos-1250000000",
		KeyPrefix:    "uploads/",
		EnforceHTTPS: true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	cond, ok := stmts[0]["condition"].(map[string]any)
	require.True(t, ok, "condition must be present when EnforceHTTPS is set")
	assert.Equal(t, map[string]any{
		"Bool": map[string]string{"qcs:secure_transport": "true"},
	}, cond)
}

// TestBuildTencentPolicy_LockObjectACL verifies the StringLike Condition on
// the COS ACL header (forces private regardless of client-supplied value).
func TestBuildTencentPolicy_LockObjectACL(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:        "photos-1250000000",
		KeyPrefix:     "uploads/",
		LockObjectACL: true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	cond, ok := stmts[0]["condition"].(map[string]any)
	require.True(t, ok, "condition must be present when LockObjectACL is set")
	assert.Equal(t, map[string]any{
		"StringLike": map[string]string{"cos:x-cos-acl": "private"},
	}, cond)
}

// TestBuildTencentPolicy_AllConditions verifies the two Condition operators
// can coexist in the same statement without colliding (different keys).
func TestBuildTencentPolicy_AllConditions(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:        "photos-1250000000",
		KeyPrefix:     "uploads/",
		EnforceHTTPS:  true,
		LockObjectACL: true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["statement"].([]map[string]any)
	cond := stmts[0]["condition"].(map[string]any)
	assert.Contains(t, cond, "Bool")
	assert.Contains(t, cond, "StringLike")
}

// TestBuildTencentPolicy_DenyPutObjectACL verifies that enabling
// DenyPutObjectACL appends a second Deny statement targeting
// name/cos:PutObjectACL on the same Resource set as the Allow statement.
func TestBuildTencentPolicy_DenyPutObjectACL(t *testing.T) {
	policy, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:            "photos-1250000000",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
		DenyPutObjectACL:  true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["statement"].([]map[string]any)
	require.Len(t, stmts, 2, "Allow + Deny statements expected")

	assert.Equal(t, "allow", stmts[0]["effect"])
	denyStmt := stmts[1]
	assert.Equal(t, "deny", denyStmt["effect"])
	assert.Equal(t, []string{"name/cos:PutObjectACL"}, denyStmt["action"])

	// Deny Resource must match the Allow Resource exactly so the deny applies
	// to what the credential could otherwise upload.
	allowRes := stmts[0]["resource"].([]string)
	denyRes := denyStmt["resource"].([]string)
	assert.Equal(t, allowRes, denyRes, "Deny Resource must match Allow Resource")
	assert.Equal(t, []string{
		"qcs::cos:*:uid/*:photos-1250000000/uploads/*.jpg",
		"qcs::cos:*:uid/*:photos-1250000000/uploads/*.png",
	}, denyRes)
}

// TestBuildTencentPolicy_NilPolicy verifies a nil policy is rejected.
func TestBuildTencentPolicy_NilPolicy(t *testing.T) {
	_, err := buildTencentPolicy(nil, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts policy")
}

// TestBuildTencentPolicy_EmptyBucket verifies a missing bucket is rejected.
func TestBuildTencentPolicy_EmptyBucket(t *testing.T) {
	_, err := buildTencentPolicy(&types.STSPolicy{
		KeyPrefix: "uploads/",
	}, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bucket is required")
}

// TestOrWildcard is a tiny table test for the helper.
func TestOrWildcard(t *testing.T) {
	assert.Equal(t, "*", orWildcard(""))
	assert.Equal(t, "ap-guangzhou", orWildcard("ap-guangzhou"))
}

// TestOrWildcardAppID verifies the uid/ prefix is added by buildTencentPolicy
// (not by this helper); the helper itself just returns the bare appID or "*".
func TestOrWildcardAppID(t *testing.T) {
	assert.Equal(t, "*", orWildcardAppID(""))
	assert.Equal(t, "1250000000", orWildcardAppID("1250000000"))
}

// TestToCredentialPolicy_RoundTripsBuildTencentPolicy verifies the SDK-policy
// translation preserves every field the buildTencentPolicy emits. This is a
// regression guard for the toCredentialPolicy / toStringSlice /
// toConditionMap helpers.
func TestToCredentialPolicy_RoundTripsBuildTencentPolicy(t *testing.T) {
	policyMap, err := buildTencentPolicy(&types.STSPolicy{
		Bucket:            "photos-1250000000",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
		EnforceHTTPS:      true,
		LockObjectACL:     true,
		DenyPutObjectACL:  true,
	}, "ap-guangzhou", "1250000000")
	require.NoError(t, err)

	sdkPolicy, err := toCredentialPolicy(policyMap)
	require.NoError(t, err)
	require.NotNil(t, sdkPolicy)

	assert.Equal(t, "2.0", sdkPolicy.Version)
	require.Len(t, sdkPolicy.Statement, 2, "Allow + Deny statements")

	allow := sdkPolicy.Statement[0]
	assert.Equal(t, "allow", allow.Effect)
	assert.Equal(t, []string{"name/cos:PostObject", "name/cos:PutObject"}, allow.Action)
	assert.Equal(t, []string{
		"qcs::cos:ap-guangzhou:uid/1250000000:photos-1250000000/uploads/*.jpg",
		"qcs::cos:ap-guangzhou:uid/1250000000:photos-1250000000/uploads/*.png",
	}, allow.Resource)
	require.NotNil(t, allow.Condition)
	assert.Contains(t, allow.Condition, "Bool")
	assert.Contains(t, allow.Condition, "StringLike")

	deny := sdkPolicy.Statement[1]
	assert.Equal(t, "deny", deny.Effect)
	assert.Equal(t, []string{"name/cos:PutObjectACL"}, deny.Action)
}

// TestToCredentialPolicy_MalformedStatement verifies the translator surfaces a
// clear error when the statement slice is missing or has the wrong type.
func TestToCredentialPolicy_MalformedStatement(t *testing.T) {
	_, err := toCredentialPolicy(map[string]any{
		"version":   "2.0",
		"statement": "not-a-slice",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "statement missing or wrong type")
}

// fakeSTS is a minimal stsClient stand-in for unit-testing GetSTSToken without
// spinning up an HTTP server.
type fakeSTS struct {
	gotReq *getCredentialReq
	resp   *getCredentialResp
	err    error
}

func (f *fakeSTS) getCredential(req *getCredentialReq) (*getCredentialResp, error) {
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// newTencentProviderWithFakeSTS bypasses the real constructor (which would
// try to init a real stsClient) and wires the fake manually. If fake is nil
// the provider's stsCli field stays a nil interface (NOT a (*fakeSTS, nil)
// boxed interface — Go's classic interface-nil gotcha), so GetSTSToken's
// nil-guard fires correctly.
//
// Note: TencentProvider is defined in provider.go (Task 5). This helper only
// sets the fields GetSTSToken reads; the cos.Client stays nil and tests in
// this file never invoke COS methods.
func newTencentProviderWithFakeSTS(fake *fakeSTS) *TencentProvider {
	p := &TencentProvider{
		endpoint:  "https://cos.ap-guangzhou.myqcloud.com",
		accessKey: "ak",
		secretKey: "sk",
		region:    "ap-guangzhou",
		appID:     "1250000000",
		// client (*cos.Client) is nil; tests don't call COS methods.
	}
	if fake != nil {
		p.stsCli = fake
	}
	return p
}

// TestTencentProvider_GetSTSToken_NoSTSClient verifies that a provider
// without an stsCli returns an explicit error rather than panicking on nil.
func TestTencentProvider_GetSTSToken_NoSTSClient(t *testing.T) {
	p := newTencentProviderWithFakeSTS(nil)
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "photos-1250000000",
		KeyPrefix: "p/",
		TTL:       30 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestTencentProvider_GetSTSToken_NilPolicy verifies a nil policy is rejected
// before the SDK is touched.
func TestTencentProvider_GetSTSToken_NilPolicy(t *testing.T) {
	fake := &fakeSTS{resp: &getCredentialResp{Expiration: "2026-06-26T15:30:00Z"}}
	p := newTencentProviderWithFakeSTS(fake)
	_, err := p.GetSTSToken(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts policy")
	assert.Nil(t, fake.gotReq, "must not call stsCli when policy is nil")
}

// TestTencentProvider_GetSTSToken_BelowTencentMinTTL verifies that a TTL
// below Tencent CAM STS's 1800s minimum is rejected locally with an
// actionable error instead of being forwarded to the SDK.
func TestTencentProvider_GetSTSToken_BelowTencentMinTTL(t *testing.T) {
	fake := &fakeSTS{resp: &getCredentialResp{Expiration: "2026-06-26T15:30:00Z"}}
	p := newTencentProviderWithFakeSTS(fake)
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "photos-1250000000",
		KeyPrefix: "p/",
		TTL:       10 * time.Minute, // 600s, below 1800s minimum
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Tencent CAM STS minimum")
	assert.Nil(t, fake.gotReq, "must not call stsCli when TTL validation fails locally")
}

// TestTencentProvider_GetSTSToken_AboveTencentMaxTTL verifies that a TTL
// above Tencent CAM STS's 7200s maximum is rejected locally.
func TestTencentProvider_GetSTSToken_AboveTencentMaxTTL(t *testing.T) {
	fake := &fakeSTS{resp: &getCredentialResp{Expiration: "2026-06-26T15:30:00Z"}}
	p := newTencentProviderWithFakeSTS(fake)
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "photos-1250000000",
		KeyPrefix: "p/",
		TTL:       3 * time.Hour, // 10800s, above 7200s maximum
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "above Tencent CAM STS maximum")
	assert.Nil(t, fake.gotReq, "must not call stsCli when TTL validation fails locally")
}

// TestTencentProvider_GetSTSToken_Success verifies the happy path.
func TestTencentProvider_GetSTSToken_Success(t *testing.T) {
	fake := &fakeSTS{
		resp: &getCredentialResp{
			RequestId:       "req-1",
			AccessKeyId:     "STS.ak",
			AccessKeySecret: "STS.sk",
			SecurityToken:   "STS.token",
			Expiration:      "2026-06-26T15:30:00Z",
		},
	}
	p := newTencentProviderWithFakeSTS(fake)

	cred, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		OwnerID:           100,
		OwnerType:         1,
		Bucket:            "photos-1250000000",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
		TTL:               30 * time.Minute,
	})
	require.NoError(t, err)

	require.NotNil(t, fake.gotReq)
	assert.Equal(t, int64(1800), fake.gotReq.DurationSeconds)
	assert.Equal(t, "2.0", fake.gotReq.Policy["version"])

	assert.Equal(t, "STS.ak", cred.AccessKey)
	assert.Equal(t, "STS.sk", cred.SecretKey)
	assert.Equal(t, "STS.token", cred.SecurityToken)
	assert.Equal(t, "https://cos.ap-guangzhou.myqcloud.com", cred.Endpoint)
	assert.Equal(t, "ap-guangzhou", cred.Region, "Region must be surfaced so clients don't derive from Endpoint")
	assert.Equal(t, "photos-1250000000", cred.Bucket)
	assert.Equal(t, "uploads/", cred.ObjectKeyPrefix)
	expectedExpiry := time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)
	assert.WithinDuration(t, expectedExpiry, cred.ExpiresAt, time.Second)
}

// TestTencentProvider_GetSTSToken_BadExpiration verifies parse failure surfaces.
func TestTencentProvider_GetSTSToken_BadExpiration(t *testing.T) {
	fake := &fakeSTS{
		resp: &getCredentialResp{
			Expiration: "not-a-date",
		},
	}
	p := newTencentProviderWithFakeSTS(fake)
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "photos-1250000000",
		KeyPrefix: "p/",
		TTL:       30 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse sts expiration")
}

// TestTencentProvider_GetSTSToken_STSErrorPropagates verifies a non-nil
// fake.err is wrapped and surfaced with the operational context prefix.
func TestTencentProvider_GetSTSToken_STSErrorPropagates(t *testing.T) {
	fake := &fakeSTS{err: errFakeSTS}
	p := newTencentProviderWithFakeSTS(fake)
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "photos-1250000000",
		KeyPrefix: "p/",
		TTL:       30 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tencent sts get credential")
	assert.ErrorIs(t, err, errFakeSTS)
}

// TestNewSTSClient_NilOpts verifies the constructor fails fast on nil opts.
func TestNewSTSClient_NilOpts(t *testing.T) {
	_, err := newSTSClient(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts client opts")
}

// TestNewSTSClient_BuildsClientWithHost verifies the constructor accepts the
// Host override without panicking and returns a non-nil client. We don't
// exercise the network path here — the SDK is opaque about its internals so
// we only assert the wrapper is non-nil and the host option is wired (the
// latter is exercised indirectly via a real GetCredential call in
// integration tests).
func TestNewSTSClient_BuildsClientWithHost(t *testing.T) {
	c, err := newSTSClient(&stsClientOpts{
		SecretID:  "ak",
		SecretKey: "sk",
		AppID:     "1250000000",
		Region:    "ap-guangzhou",
		Host:      "sts.tencentcloudapi.com",
	})
	require.NoError(t, err)
	require.NotNil(t, c)
	require.NotNil(t, c.cli, "SDK client must be initialized")
}
