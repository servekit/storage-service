package huawei

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
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
	assert.Equal(t, []string{"OBS:*:*:object:photos/uploads/*"}, stmts[0]["Resource"])
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
		"OBS:*:*:object:photos/uploads/*.jpg",
		"OBS:*:*:object:photos/uploads/*.png",
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
		assert.Equal(t, []string{"OBS:*:*:object:photos/uploads/*"}, resources,
			"prefix %q should normalize", prefix)
	}
}

// TestBuildHuaweiPolicy_EmptyOrSlashKeyPrefix verifies that an empty or
// "/" KeyPrefix produces a single-slash resource base. Without this guard
// the format string yields "OBS:*:*:object:bucket//*" (double slash) which Huawei
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
		assert.Equal(t, []string{"OBS:*:*:object:photos/*"}, resources,
			"prefix %q should normalize to bucket-only resource", prefix)
	}
}

// TestBuildHuaweiPolicy_EnforceHTTPS verifies the Bool Condition that
// blocks plaintext HTTP uploads at OBS. Note: Huawei's condition key is
// "SecureTransport" (NOT Aliyun's "acs:SecureTransport"). The SDK's
// Condition field is map[string]map[string][]string, so values are wrapped
// in single-element slices to round-trip cleanly into model.ServicePolicy.
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
		"Bool": map[string][]string{"SecureTransport": {"true"}},
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
		"StringEquals": map[string][]string{"obs:objectAcl": {"private"}},
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
		"OBS:*:*:object:photos/uploads/*.jpg",
		"OBS:*:*:object:photos/uploads/*.png",
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
			"Resource": ["OBS:*:*:object:photos/uploads/*"]
		}]
	}`
	assert.JSONEq(t, want, string(got))
}

// TestBuildHuaweiPolicy_JSONEq_FullHardening verifies the full hardened
// policy shape via assert.JSONEq: Allow + Condition (Bool+StringEquals) +
// Deny statement. The Condition values use single-element arrays to match
// the SDK's map[string]map[string][]string shape; assert.JSONEq compares
// semantically so a JSON "true" matches ["true"] in our test fixture via
// the marshaler (we pass the slice form on both sides here).
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
				"Resource": ["OBS:*:*:object:photos/uploads/*.jpg"],
				"Condition": {
					"Bool": {"SecureTransport": ["true"]},
					"StringEquals": {"obs:objectAcl": ["private"]}
				}
			},
			{
				"Effect": "Deny",
				"Action": ["obs:object:PutObjectAcl"],
				"Resource": ["OBS:*:*:object:photos/uploads/*.jpg"]
			}
		]
	}`
	assert.JSONEq(t, want, string(got))
}

// TestBuildServicePolicy_RoundTrip verifies the loose map[string]any
// produced by buildHuaweiPolicy round-trips cleanly into the SDK's typed
// *model.ServicePolicy. This is the translation step assumeAgency uses to
// hand the policy to CreateTemporaryAccessKeyByAgency. Without this test
// a drift between our map shape and the SDK struct tags (e.g. an
// unexpected Condition value type) would only surface as a runtime
// unmarshal error inside the SDK call.
func TestBuildServicePolicy_RoundTrip(t *testing.T) {
	policy, err := buildHuaweiPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg"},
		EnforceHTTPS:      true,
		DenyPutObjectACL:  true,
	})
	require.NoError(t, err)

	typed, err := buildServicePolicy(policy)
	require.NoError(t, err)
	assert.Equal(t, "1.1", typed.Version)
	require.Len(t, typed.Statement, 2, "Allow + Deny statements")
	// Action / Resource / Effect are the load-bearing fields; verifying
	// them proves the JSON tags on model.ServiceStatement line up with
	// our map keys.
	assert.Equal(t, []string{"obs:object:PutObject"}, typed.Statement[0].Action)
	assert.Equal(t, []string{"OBS:*:*:object:photos/uploads/*.jpg"}, *typed.Statement[0].Resource)
	// Statement[0].Condition is map[string]map[string][]string.
	boolCond, ok := typed.Statement[0].Condition["Bool"]
	require.True(t, ok, "Bool condition should round-trip")
	assert.Equal(t, []string{"true"}, boolCond["SecureTransport"])
}

// TestBuildServicePolicy_MissingVersion verifies the validator rejects
// policies without a Version field. Guards against accidentally feeding a
// raw STSPolicy map straight to the SDK.
func TestBuildServicePolicy_MissingVersion(t *testing.T) {
	_, err := buildServicePolicy(map[string]any{
		"Statement": []map[string]any{{"Effect": "Allow"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Version")
}

// TestBuildServicePolicy_MissingStatement verifies the validator rejects
// policies without any Statement.
func TestBuildServicePolicy_MissingStatement(t *testing.T) {
	_, err := buildServicePolicy(map[string]any{
		"Version": "1.1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Statement")
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

// TestParseHuaweiExpiry verifies the IAM expires_at parser accepts the
// canonical RFC3339 layout IAM returns. Empty input and garbage are
// rejected so callers don't silently trust a zero time.
func TestParseHuaweiExpiry(t *testing.T) {
	t.Run("rfc3339", func(t *testing.T) {
		got, err := parseHuaweiExpiry("2026-06-26T15:30:00Z")
		require.NoError(t, err)
		assert.False(t, got.IsZero())
	})
	t.Run("rfc3339_nanos", func(t *testing.T) {
		// IAM commonly returns sub-second precision.
		got, err := parseHuaweiExpiry("2026-06-26T15:30:00.123456Z")
		require.NoError(t, err)
		assert.False(t, got.IsZero())
	})
	t.Run("empty", func(t *testing.T) {
		_, err := parseHuaweiExpiry("")
		require.Error(t, err)
	})
	t.Run("garbage", func(t *testing.T) {
		_, err := parseHuaweiExpiry("not-a-timestamp")
		require.Error(t, err)
	})
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

// TestNewSTSClient_CustomEndpoint verifies the happy path with a custom
// endpoint (the tests path — exercises the WithEndpoint branch and
// confirms the credentials + HTTP config build cleanly).
func TestNewSTSClient_CustomEndpoint(t *testing.T) {
	c, err := newSTSClient(&stsClientOpts{
		AccessKey: "ak",
		SecretKey: "sk",
		DomainID:  "demo-account",
		Region:    "cn-north-4",
		Endpoint:  "https://iam.example.com",
	})
	require.NoError(t, err)
	assert.NotNil(t, c)
	assert.NotNil(t, c.cli)
}

// fakeSTS is a minimal assumeAgencyCaller stand-in for unit-testing
// GetSTSToken without spinning up an HTTP server. (Currently used by
// Task 5's provider tests; defined here so the type is available when
// those tests land.)
type fakeSTS struct {
	gotReq *assumeAgencyReq
	resp   *assumeAgencyResp
	err    error
}

func (f *fakeSTS) assumeAgency(ctx context.Context, req *assumeAgencyReq) (*assumeAgencyResp, error) {
	_ = ctx
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// Compile-time check that *fakeSTS satisfies assumeAgencyCaller.
var _ assumeAgencyCaller = (*fakeSTS)(nil)

// newTestSTSClient builds a real *stsClient against a non-routable endpoint.
// It exists so that assumeAgency guards which short-circuit before the SDK
// call (e.g. the DurationSeconds bounds check) can be exercised without
// spinning up an httptest.Server. Any test path that actually reaches the
// SDK request will fail fast on the bad host — that's intentional, the
// guards we test here must reject before any network I/O.
func newTestSTSClient(t *testing.T) *stsClient {
	t.Helper()
	c, err := newSTSClient(&stsClientOpts{
		AccessKey: "ak",
		SecretKey: "sk",
		DomainID:  "demo-account",
		Region:    "cn-north-4",
		Endpoint:  "https://iam.example.invalid",
	})
	require.NoError(t, err)
	return c
}

// TestAssumeAgency_BelowMinDuration verifies assumeAgency rejects
// DurationSeconds < 900 before forwarding to the SDK, so callers get a clear
// local error instead of a wrapped IAM backend failure.
func TestAssumeAgency_BelowMinDuration(t *testing.T) {
	c := newTestSTSClient(t)
	_, err := c.assumeAgency(context.Background(), &assumeAgencyReq{
		AgencyName:      "test-agency",
		DomainID:        "demo-account",
		RoleSessionName: "owner-1",
		DurationSeconds: minHuaweiSTSDuration - 1,
		Policy: map[string]any{
			"Version":   "1.1",
			"Statement": []map[string]any{{"Effect": "Allow"}},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "below min")
}

// TestAssumeAgency_AboveMaxDuration verifies assumeAgency rejects
// DurationSeconds > 43200 before forwarding to the SDK.
func TestAssumeAgency_AboveMaxDuration(t *testing.T) {
	c := newTestSTSClient(t)
	_, err := c.assumeAgency(context.Background(), &assumeAgencyReq{
		AgencyName:      "test-agency",
		DomainID:        "demo-account",
		RoleSessionName: "owner-1",
		DurationSeconds: maxHuaweiSTSDuration + 1,
		Policy: map[string]any{
			"Version":   "1.1",
			"Statement": []map[string]any{{"Effect": "Allow"}},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "above max")
}

// TestAssumeAgency_MinMaxBoundsAccepted confirms the boundary values
// themselves are NOT rejected — only values strictly outside [900, 43200].
// These pass the bounds check and proceed to the SDK call, which then fails
// on the non-routable test endpoint. We assert the error does NOT mention
// "below min" / "above max" so we know the bounds guard passed.
func TestAssumeAgency_MinMaxBoundsAccepted(t *testing.T) {
	c := newTestSTSClient(t)
	for _, dur := range []int32{minHuaweiSTSDuration, maxHuaweiSTSDuration} {
		_, err := c.assumeAgency(context.Background(), &assumeAgencyReq{
			AgencyName:      "test-agency",
			DomainID:        "demo-account",
			RoleSessionName: "owner-1",
			DurationSeconds: dur,
			Policy: map[string]any{
				"Version":   "1.1",
				"Statement": []map[string]any{{"Effect": "Allow"}},
			},
		})
		require.Error(t, err, "expected SDK/network failure at boundary dur=%d", dur)
		assert.NotContains(t, err.Error(), "below min")
		assert.NotContains(t, err.Error(), "above max")
	}
}
