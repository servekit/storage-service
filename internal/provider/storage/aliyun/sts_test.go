package aliyun

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// TestBuildAliyunPolicy_NoExtensions verifies empty AllowedExtensions yields
// a single Resource wildcard covering the entire prefix. Empty region/accountUID
// preserve legacy wildcard ("acs:oss:*:*:...") semantics.
func TestBuildAliyunPolicy_NoExtensions(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:    "photos",
		KeyPrefix: "uploads/",
	}, "", "")
	require.NoError(t, err)

	assert.Equal(t, "1", policy["Version"])
	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	assert.Equal(t, "Allow", stmts[0]["Effect"])
	assert.Equal(t, []string{"oss:PutObject"}, stmts[0]["Action"])
	assert.Equal(t, []string{"acs:oss:*:*:photos/uploads/*"}, stmts[0]["Resource"])
	// No hardening flags set → Condition must be absent (Aliyun RAM rejects
	// an empty Condition block).
	_, hasCond := stmts[0]["Condition"]
	assert.False(t, hasCond, "Condition should be absent when no hardening flags set")
}

// TestBuildAliyunPolicy_WithExtensions verifies each extension becomes a
// separate Resource wildcard entry.
func TestBuildAliyunPolicy_WithExtensions(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
	}, "", "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	resources := stmts[0]["Resource"].([]string)
	assert.Equal(t, []string{
		"acs:oss:*:*:photos/uploads/*.jpg",
		"acs:oss:*:*:photos/uploads/*.png",
	}, resources)
}

// TestBuildAliyunPolicy_BadExtensionFormat verifies extensions missing the
// '.' prefix are rejected.
func TestBuildAliyunPolicy_BadExtensionFormat(t *testing.T) {
	_, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{"jpg"},
	}, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with '.'")
}

// TestBuildAliyunPolicy_CustomActions verifies AllowedActions override default.
func TestBuildAliyunPolicy_CustomActions(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:         "photos",
		KeyPrefix:      "uploads/",
		AllowedActions: []string{"oss:PutObject", "oss:GetObject"},
	}, "", "")
	require.NoError(t, err)
	stmts := policy["Statement"].([]map[string]any)
	assert.Equal(t, []string{"oss:PutObject", "oss:GetObject"}, stmts[0]["Action"])
}

// TestBuildAliyunPolicy_KeyPrefixTrailingSlashStripped verifies prefix
// normalization (no double slash).
func TestBuildAliyunPolicy_KeyPrefixTrailingSlashStripped(t *testing.T) {
	for _, prefix := range []string{"uploads/", "uploads"} {
		policy, err := buildAliyunPolicy(&types.STSPolicy{
			Bucket:    "photos",
			KeyPrefix: prefix,
		}, "", "")
		require.NoError(t, err)
		stmts := policy["Statement"].([]map[string]any)
		resources := stmts[0]["Resource"].([]string)
		assert.Equal(t, []string{"acs:oss:*:*:photos/uploads/*"}, resources,
			"prefix %q should normalize", prefix)
	}
}

// TestBuildAliyunPolicy_EmptyOrSlashKeyPrefix verifies that an empty or
// "/" KeyPrefix produces a single-slash resource base. Without this guard
// the format string yields "acs:oss:*:*:bucket//*" (double slash) which
// Aliyun RAM matches literally and silently rejects at PUT time.
func TestBuildAliyunPolicy_EmptyOrSlashKeyPrefix(t *testing.T) {
	for _, prefix := range []string{"", "/", "//"} {
		policy, err := buildAliyunPolicy(&types.STSPolicy{
			Bucket:    "photos",
			KeyPrefix: prefix,
		}, "", "")
		require.NoError(t, err)
		stmts := policy["Statement"].([]map[string]any)
		resources := stmts[0]["Resource"].([]string)
		assert.Equal(t, []string{"acs:oss:*:*:photos/*"}, resources,
			"prefix %q should normalize to bucket-only resource", prefix)
	}
}

// TestBuildAliyunPolicy_RegionAndAccountScope verifies that non-empty region
// and accountUID tighten the Resource ARN from wildcard to scoped values.
// This is the core credential-hardening guarantee: a leaked STS token cannot
// be replayed against same-named buckets in other regions or accounts.
func TestBuildAliyunPolicy_RegionAndAccountScope(t *testing.T) {
	cases := []struct {
		name       string
		region     string
		accountUID string
		wantBase   string
	}{
		{"both empty → wildcard", "", "", "acs:oss:*:*:photos/uploads/*"},
		{"region only", "cn-hangzhou", "", "acs:oss:cn-hangzhou:*:photos/uploads/*"},
		{"account only", "", "123456", "acs:oss:*:123456:photos/uploads/*"},
		{"both scoped", "cn-hangzhou", "123456", "acs:oss:cn-hangzhou:123456:photos/uploads/*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := buildAliyunPolicy(&types.STSPolicy{
				Bucket:    "photos",
				KeyPrefix: "uploads/",
			}, tc.region, tc.accountUID)
			require.NoError(t, err)
			stmts := policy["Statement"].([]map[string]any)
			resources := stmts[0]["Resource"].([]string)
			assert.Equal(t, []string{tc.wantBase}, resources)
		})
	}
}

// TestBuildAliyunPolicy_EnforceHTTPS verifies the Bool Condition that blocks
// plaintext HTTP uploads at OSS.
func TestBuildAliyunPolicy_EnforceHTTPS(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:       "photos",
		KeyPrefix:    "uploads/",
		EnforceHTTPS: true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	cond, ok := stmts[0]["Condition"].(map[string]any)
	require.True(t, ok, "Condition must be present when EnforceHTTPS is set")
	assert.Equal(t, map[string]any{
		"Bool": map[string]string{"acs:SecureTransport": "true"},
	}, cond)
}

// TestBuildAliyunPolicy_LockObjectACL verifies the StringEquals Condition that
// forces uploaded objects to "private" regardless of client-supplied ACL headers.
func TestBuildAliyunPolicy_LockObjectACL(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:        "photos",
		KeyPrefix:     "uploads/",
		LockObjectACL: true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	cond, ok := stmts[0]["Condition"].(map[string]any)
	require.True(t, ok, "Condition must be present when LockObjectACL is set")
	assert.Equal(t, map[string]any{
		"StringEquals": map[string]string{"oss:x-oss-object-acl": "private"},
	}, cond)
}

// TestBuildAliyunPolicy_AllConditions verifies the two Condition operators can
// coexist in the same statement without colliding (they use different keys).
func TestBuildAliyunPolicy_AllConditions(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:        "photos",
		KeyPrefix:     "uploads/",
		EnforceHTTPS:  true,
		LockObjectACL: true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	cond := stmts[0]["Condition"].(map[string]any)
	assert.Contains(t, cond, "Bool")
	assert.Contains(t, cond, "StringEquals")
}

// TestBuildAliyunPolicy_DenyPutObjectACL verifies that enabling DenyPutObjectACL
// appends a second Deny statement targeting oss:PutObjectAcl on the same
// Resource set as the Allow statement.
func TestBuildAliyunPolicy_DenyPutObjectACL(t *testing.T) {
	policy, err := buildAliyunPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
		DenyPutObjectACL:  true,
	}, "", "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 2, "Allow + Deny statements expected")

	assert.Equal(t, "Allow", stmts[0]["Effect"])
	denyStmt := stmts[1]
	assert.Equal(t, "Deny", denyStmt["Effect"])
	assert.Equal(t, []string{"oss:PutObjectAcl"}, denyStmt["Action"])

	// Deny Resource must match the Allow Resource exactly so the deny applies
	// to what the credential could otherwise upload.
	allowRes := stmts[0]["Resource"].([]string)
	denyRes := denyStmt["Resource"].([]string)
	assert.Equal(t, allowRes, denyRes, "Deny Resource must match Allow Resource")
	assert.Equal(t, []string{
		"acs:oss:*:*:photos/uploads/*.jpg",
		"acs:oss:*:*:photos/uploads/*.png",
	}, denyRes)
}

// TestParseAccountUID covers extraction from RAM role ARNs of varying shape.
// Returns "" on malformed input so callers fall back to wildcard Resource
// rather than producing nonsense.
func TestParseAccountUID(t *testing.T) {
	cases := []struct {
		name string
		arn  string
		want string
	}{
		{"empty", "", ""},
		{"canonical RAM ARN", "acs:ram::1234567890:role/uploader", "1234567890"},
		{"missing acs prefix", "ram::1234:role/uploader", ""},
		{"non-ram ARN", "acs:oss::1234:bucket", ""},
		{"too few segments", "acs:ram", ""},
		{"empty UID still parses", "acs:ram:::role/uploader", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseAccountUID(tc.arn))
		})
	}
}

// TestOrWildcard is a tiny table test for the helper.
func TestOrWildcard(t *testing.T) {
	assert.Equal(t, "*", orWildcard(""))
	assert.Equal(t, "cn-hangzhou", orWildcard("cn-hangzhou"))
}

func TestStsEndpointFor(t *testing.T) {
	cases := []struct {
		region string
		want   string
	}{
		{"cn-hangzhou", "sts.cn-hangzhou.aliyuncs.com"},
		{"us-west-1", "sts.us-west-1.aliyuncs.com"},
		{"", "sts.cn-hangzhou.aliyuncs.com"},
	}
	for _, tc := range cases {
		t.Run(tc.region, func(t *testing.T) {
			assert.Equal(t, tc.want, stsEndpointFor(tc.region))
		})
	}
}

// fakeSTS is a minimal stsClient stand-in for unit-testing GetSTSToken without
// spinning up an HTTP server.
type fakeSTS struct {
	gotReq *assumeRoleReq
	resp   *assumeRoleResp
	err    error
}

func (f *fakeSTS) assumeRole(req *assumeRoleReq) (*assumeRoleResp, error) {
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// newAliyunProviderWithFakeSTS bypasses the real constructor (which would try
// to init a real stsClient) and wires the fake manually. If fake is nil the
// provider's stsCli field stays a nil interface (NOT a (*fakeSTS, nil) boxed
// interface — Go's classic interface-nil gotcha), so GetSTSToken's nil-guard
// fires correctly.
func newAliyunProviderWithFakeSTS(fake *fakeSTS, roleARN string) *AliyunProvider {
	p := &AliyunProvider{
		endpoint:  "https://oss.example.com",
		accessKey: "ak",
		secretKey: "sk",
		region:    "cn-hangzhou",
		roleARN:   roleARN,
		// client (oss.Client) is nil; tests don't call OSS methods.
	}
	if fake != nil {
		p.stsCli = fake
	}
	return p
}

// TestAliyunProvider_GetSTSToken_NoRoleARN verifies that a provider without
// RoleARN returns an explicit error rather than panicking on nil stsCli.
func TestAliyunProvider_GetSTSToken_NoRoleARN(t *testing.T) {
	p := newAliyunProviderWithFakeSTS(nil, "")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestAliyunProvider_GetSTSToken_STSClientNilButRoleARNSet covers the
// defensive branch where roleARN is set but stsCli is nil — should not happen
// via the constructor (which initialises stsCli whenever roleARN != ""), but
// the nil-guard must still produce the "not configured" error instead of
// panicking on a nil dereference.
func TestAliyunProvider_GetSTSToken_STSClientNilButRoleARNSet(t *testing.T) {
	p := newAliyunProviderWithFakeSTS(nil, "acs:ram::1:role/r")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestAliyunProvider_GetSTSToken_BelowAliyunMinTTL verifies that a TTL below
// Aliyun's 900s AssumeRole minimum is rejected locally with an actionable
// error instead of being forwarded to the SDK (which returns an opaque API
// failure).
func TestAliyunProvider_GetSTSToken_BelowAliyunMinTTL(t *testing.T) {
	fake := &fakeSTS{resp: &assumeRoleResp{Expiration: "2026-06-23T15:30:00Z"}}
	p := newAliyunProviderWithFakeSTS(fake, "acs:ram::1:role/r")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       5 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Aliyun AssumeRole minimum")
	assert.Nil(t, fake.gotReq, "must not call stsCli when TTL validation fails locally")
}

// TestAliyunProvider_GetSTSToken_Success verifies happy path.
func TestAliyunProvider_GetSTSToken_Success(t *testing.T) {
	fake := &fakeSTS{
		resp: &assumeRoleResp{
			RequestId:       "req-1",
			AccessKeyId:     "STS.ak",
			AccessKeySecret: "STS.sk",
			SecurityToken:   "STS.token",
			Expiration:      "2026-06-23T15:30:00Z",
		},
	}
	p := newAliyunProviderWithFakeSTS(fake, "acs:ram::1234:role/uploader")

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
	assert.Equal(t, "acs:ram::1234:role/uploader", fake.gotReq.RoleArn)
	require.NotNil(t, fake.gotReq.DurationSeconds)
	assert.Equal(t, int64(900), *fake.gotReq.DurationSeconds)

	assert.Equal(t, "1", fake.gotReq.Policy["Version"])

	assert.Equal(t, "STS.ak", cred.AccessKey)
	assert.Equal(t, "STS.sk", cred.SecretKey)
	assert.Equal(t, "STS.token", cred.SecurityToken)
	assert.Equal(t, "https://oss.example.com", cred.Endpoint)
	assert.Equal(t, "cn-hangzhou", cred.Region, "Region must be surfaced so clients don't derive from Endpoint")
	assert.Equal(t, "photos", cred.Bucket)
	assert.Equal(t, "uploads/", cred.ObjectKeyPrefix)
	expectedExpiry := time.Date(2026, 6, 23, 15, 30, 0, 0, time.UTC)
	assert.WithinDuration(t, expectedExpiry, cred.ExpiresAt, time.Second)
}

// TestAliyunProvider_GetSTSToken_BadExpiration verifies parse failure surfaces.
func TestAliyunProvider_GetSTSToken_BadExpiration(t *testing.T) {
	fake := &fakeSTS{
		resp: &assumeRoleResp{
			Expiration: "not-a-date",
		},
	}
	p := newAliyunProviderWithFakeSTS(fake, "acs:ram::1:role/r")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse sts expiration")
}

// --- stsClient tests (HTTP-mocked AssumeRole) ---

// hostFromURL strips the scheme from a URL, returning "host:port". The Aliyun
// SDK expects Endpoint to be a bare host (it composes the final URL from
// Protocol + Endpoint), so httptest.Server.URL ("http://127.0.0.1:port") must
// be stripped before being passed to stsClientOpts.Endpoint.
func hostFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}

// TestAssumeRole_Success mocks the STS API and verifies the response mapping.
// The STS SDK puts AssumeRole parameters in the URL query string (RPC style);
// the test reads them from r.URL.Query() — Policy must arrive as a JSON string
// with no HTML escaping.
func TestAssumeRole_Success(t *testing.T) {
	var capturedQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"RequestId": "req-123",
			"Credentials": {
				"AccessKeyId": "STS.ak123",
				"AccessKeySecret": "STS.sk123",
				"SecurityToken": "STS.token456",
				"Expiration": "2026-06-23T15:30:00Z"
			}
		}`))
	}))
	defer srv.Close()

	c, err := newSTSClient(&stsClientOpts{
		AccessKeyId:     "ak",
		AccessKeySecret: "sk",
		RegionId:        "cn-hangzhou",
		Endpoint:        hostFromURL(t, srv.URL),
		Protocol:        "http",
	})
	require.NoError(t, err)

	duration := int64(900)
	resp, err := c.assumeRole(&assumeRoleReq{
		RoleArn:         "acs:ram::1234:role/test",
		RoleSessionName: "owner-100",
		DurationSeconds: &duration,
		Policy: map[string]any{
			"Version": "1",
			"Statement": []map[string]any{{
				"Effect":   "Allow",
				"Action":   []string{"oss:PutObject"},
				"Resource": []string{"acs:oss:*:*:bucket/uploads/*"},
			}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "req-123", resp.RequestId)
	assert.Equal(t, "STS.ak123", resp.AccessKeyId)
	assert.Equal(t, "STS.sk123", resp.AccessKeySecret)
	assert.Equal(t, "STS.token456", resp.SecurityToken)
	assert.Equal(t, "2026-06-23T15:30:00Z", resp.Expiration)

	// Policy must be sent as a JSON string with no HTML escaping.
	policyStr := capturedQuery.Get("Policy")
	require.NotEmpty(t, policyStr, "Policy must be present in query string")
	assert.Contains(t, policyStr, `"Effect":"Allow"`)
	assert.NotContains(t, policyStr, `<`, "policy JSON must not HTML-escape")
	assert.Equal(t, "acs:ram::1234:role/test", capturedQuery.Get("RoleArn"))
	assert.Equal(t, "owner-100", capturedQuery.Get("RoleSessionName"))
	assert.Equal(t, "900", capturedQuery.Get("DurationSeconds"))
}

// TestAssumeRole_APIError verifies SDK errors get wrapped with a clear prefix.
func TestAssumeRole_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"Code":"NoPermission","Message":"unauthorized"}`))
	}))
	defer srv.Close()

	c, err := newSTSClient(&stsClientOpts{
		AccessKeyId:     "ak",
		AccessKeySecret: "sk",
		RegionId:        "cn-hangzhou",
		Endpoint:        hostFromURL(t, srv.URL),
		Protocol:        "http",
	})
	require.NoError(t, err)

	duration := int64(900)
	_, err = c.assumeRole(&assumeRoleReq{
		RoleArn:         "acs:ram::1234:role/test",
		RoleSessionName: "owner-100",
		DurationSeconds: &duration,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "assume role")
}

// TestNewSTSClient_NilOpts verifies the constructor fails fast on nil opts.
func TestNewSTSClient_NilOpts(t *testing.T) {
	_, err := newSTSClient(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts client opts")
}
