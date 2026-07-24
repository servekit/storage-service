package volcengine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// TestBuildVolcPolicy_NoExtensions verifies empty AllowedExtensions yields a
// single Resource wildcard covering the entire prefix.
func TestBuildVolcPolicy_NoExtensions(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:    "photos",
		KeyPrefix: "uploads/",
	}, "")
	require.NoError(t, err)

	_, hasVersion := policy["Version"]
	assert.False(t, hasVersion, "Volcengine policy has no Version field (only Statement)")

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	assert.Equal(t, "Allow", stmts[0]["Effect"])
	assert.Equal(t, []string{"tos:PutObject"}, stmts[0]["Action"])
	assert.Equal(t, []string{"trn:tos:::photos/uploads/*"}, stmts[0]["Resource"])

	_, hasCond := stmts[0]["Condition"]
	assert.False(t, hasCond, "Condition should be absent when no hardening flags set")
}

// TestBuildVolcPolicy_WithAccount verifies account is embedded into the
// Resource TRN.
func TestBuildVolcPolicy_WithAccount(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:    "photos",
		KeyPrefix: "uploads/",
	}, "100200300")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	resources := stmts[0]["Resource"].([]string)
	assert.Equal(t, []string{"trn:tos::100200300:photos/uploads/*"}, resources)
}

// TestBuildVolcPolicy_WithExtensions verifies each extension becomes a
// separate Resource entry.
func TestBuildVolcPolicy_WithExtensions(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
	}, "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	resources := stmts[0]["Resource"].([]string)
	assert.Equal(t, []string{
		"trn:tos:::photos/uploads/*.jpg",
		"trn:tos:::photos/uploads/*.png",
	}, resources)
}

// TestBuildVolcPolicy_BadExtensionFormat verifies extensions missing the
// '.' prefix are rejected.
func TestBuildVolcPolicy_BadExtensionFormat(t *testing.T) {
	_, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{"jpg"},
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with '.'")
}

// TestBuildVolcPolicy_CustomActions verifies AllowedActions override default.
func TestBuildVolcPolicy_CustomActions(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:         "photos",
		KeyPrefix:      "uploads/",
		AllowedActions: []string{"tos:PutObject", "tos:GetObject"},
	}, "")
	require.NoError(t, err)
	stmts := policy["Statement"].([]map[string]any)
	assert.Equal(t, []string{"tos:PutObject", "tos:GetObject"}, stmts[0]["Action"])
}

// TestBuildVolcPolicy_EmptyOrSlashKeyPrefix verifies no double-slash.
func TestBuildVolcPolicy_EmptyOrSlashKeyPrefix(t *testing.T) {
	for _, prefix := range []string{"", "/", "//"} {
		policy, err := buildVolcPolicy(&types.STSPolicy{
			Bucket:    "photos",
			KeyPrefix: prefix,
		}, "")
		require.NoError(t, err)
		stmts := policy["Statement"].([]map[string]any)
		resources := stmts[0]["Resource"].([]string)
		assert.Equal(t, []string{"trn:tos:::photos/*"}, resources,
			"prefix %q should normalize to bucket-only resource", prefix)
	}
}

// TestBuildVolcPolicy_EnforceHTTPS verifies the Bool Condition that blocks
// plaintext HTTP uploads at TOS.
func TestBuildVolcPolicy_EnforceHTTPS(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:       "photos",
		KeyPrefix:    "uploads/",
		EnforceHTTPS: true,
	}, "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	cond, ok := stmts[0]["Condition"].(map[string]any)
	require.True(t, ok, "Condition must be present when EnforceHTTPS is set")
	assert.Equal(t, map[string]any{
		"Bool": map[string]string{"tos:SecureTransport": "true"},
	}, cond)
}

// TestBuildVolcPolicy_LockObjectACL verifies the StringEquals Condition that
// forces uploaded objects to "private".
func TestBuildVolcPolicy_LockObjectACL(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:        "photos",
		KeyPrefix:     "uploads/",
		LockObjectACL: true,
	}, "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 1)
	cond, ok := stmts[0]["Condition"].(map[string]any)
	require.True(t, ok, "Condition must be present when LockObjectACL is set")
	assert.Equal(t, map[string]any{
		"StringEquals": map[string]string{"tos:x-tos-acl": "private"},
	}, cond)
}

// TestBuildVolcPolicy_DenyPutObjectACL verifies that enabling DenyPutObjectACL
// appends a second Deny statement targeting tos:PutObjectACL on the same
// Resource set.
func TestBuildVolcPolicy_DenyPutObjectACL(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:            "photos",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{".jpg", ".png"},
		DenyPutObjectACL:  true,
	}, "")
	require.NoError(t, err)

	stmts := policy["Statement"].([]map[string]any)
	require.Len(t, stmts, 2, "Allow + Deny statements expected")

	assert.Equal(t, "Allow", stmts[0]["Effect"])
	denyStmt := stmts[1]
	assert.Equal(t, "Deny", denyStmt["Effect"])
	assert.Equal(t, []string{"tos:PutObjectACL"}, denyStmt["Action"])

	allowRes := stmts[0]["Resource"].([]string)
	denyRes := denyStmt["Resource"].([]string)
	assert.Equal(t, allowRes, denyRes, "Deny Resource must match Allow Resource")
	assert.Equal(t, []string{
		"trn:tos:::photos/uploads/*.jpg",
		"trn:tos:::photos/uploads/*.png",
	}, denyRes)
}

// TestBuildVolcPolicy_JSONEq locks the wire-format JSON for the default
// (no-hardening) case. Catches accidental field reordering or case changes.
func TestBuildVolcPolicy_JSONEq(t *testing.T) {
	policy, err := buildVolcPolicy(&types.STSPolicy{
		Bucket:    "photos",
		KeyPrefix: "uploads/",
	}, "")
	require.NoError(t, err)

	raw, err := marshalPolicyJSON(policy)
	require.NoError(t, err)

	// Statement-only payload — no Version field per Volcengine docs.
	expected := `{
		"Statement": [
			{
				"Effect": "Allow",
				"Action": ["tos:PutObject"],
				"Resource": ["trn:tos:::photos/uploads/*"]
			}
		]
	}`
	assert.JSONEq(t, expected, string(raw))
}

// TestParseVolcAccount covers extraction from IAM role TRNs of varying shape.
func TestParseVolcAccount(t *testing.T) {
	cases := []struct {
		name string
		trn  string
		want string
	}{
		{"empty", "", ""},
		{"canonical IAM TRN", "trn:iam::1234567890:role/uploader", "1234567890"},
		{"missing trn prefix", "iam::1234:role/uploader", ""},
		{"non-iam TRN", "trn:tos::1234:bucket", ""},
		{"too few segments", "trn:iam", ""},
		{"empty UID still parses", "trn:iam:::role/uploader", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseVolcAccount(tc.trn))
		})
	}
}

// fakeSTS is a minimal stsClient stand-in for unit-testing GetSTSToken without
// spinning up an HTTP server.
type fakeSTS struct {
	gotCtx context.Context
	gotReq *assumeRoleReq
	resp   *assumeRoleResp
	err    error
}

func (f *fakeSTS) assumeRole(ctx context.Context, req *assumeRoleReq) (*assumeRoleResp, error) {
	f.gotCtx = ctx
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

// newVolcProviderWithFakeSTS bypasses the real constructor (which would init
// a real stsClient) and wires the fake manually. If fake is nil the provider's
// stsCli field stays a nil interface so GetSTSToken's nil-guard fires correctly.
func newVolcProviderWithFakeSTS(fake *fakeSTS, roleTRN string) *VolcengineProvider {
	p := &VolcengineProvider{
		endpoint:  "tos-cn-beijing.volces.com",
		accessKey: "ak",
		secretKey: "sk",
		region:    "cn-beijing",
		roleTRN:   roleTRN,
	}
	if fake != nil {
		p.stsCli = fake
	}
	return p
}

// TestVolcProvider_GetSTSToken_NoRoleTRN verifies a provider without RoleTRN
// returns an explicit error rather than panicking on nil stsCli.
//
// NOTE: GetSTSToken is implemented in Task 5. This test (and the other
// TestVolcProvider_GetSTSToken_* tests below) will fail at compile time until
// Task 5 lands. They are kept here (rather than deferred to Task 5) because
// the Task 4 plan defines them and Task 5 wires GetSTSToken onto the same
// struct.
func TestVolcProvider_GetSTSToken_NoRoleTRN(t *testing.T) {
	p := newVolcProviderWithFakeSTS(nil, "")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestVolcProvider_GetSTSToken_STSClientNilButRoleTRNSet covers the defensive
// branch where roleTRN is set but stsCli is nil.
func TestVolcProvider_GetSTSToken_STSClientNilButRoleTRNSet(t *testing.T) {
	p := newVolcProviderWithFakeSTS(nil, "trn:iam::1:role/r")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestVolcProvider_GetSTSToken_BelowVolcMinTTL verifies a TTL below the 900s
// minimum is rejected locally with an actionable error.
func TestVolcProvider_GetSTSToken_BelowVolcMinTTL(t *testing.T) {
	fake := &fakeSTS{resp: &assumeRoleResp{Expiration: "2026-06-26T15:30:00Z"}}
	p := newVolcProviderWithFakeSTS(fake, "trn:iam::1:role/r")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       5 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Volcengine AssumeRole minimum")
	assert.Nil(t, fake.gotReq, "must not call stsCli when TTL validation fails locally")
}

// TestVolcProvider_GetSTSToken_Success verifies the happy path.
func TestVolcProvider_GetSTSToken_Success(t *testing.T) {
	fake := &fakeSTS{
		resp: &assumeRoleResp{
			ResponseId:      "req-1",
			AccessKeyId:     "STS.ak",
			AccessKeySecret: "STS.sk",
			SessionToken:    "STS.token",
			Expiration:      "2026-06-26T15:30:00Z",
		},
	}
	p := newVolcProviderWithFakeSTS(fake, "trn:iam::1234:role/uploader")

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
	assert.Equal(t, "trn:iam::1234:role/uploader", fake.gotReq.RoleTrn)
	require.NotNil(t, fake.gotReq.DurationSeconds)
	assert.Equal(t, int32(900), *fake.gotReq.DurationSeconds)

	// Volcengine policy is Statement-only (no Version).
	_, hasVersion := fake.gotReq.Policy["Version"]
	assert.False(t, hasVersion)

	assert.Equal(t, "STS.ak", cred.AccessKey)
	assert.Equal(t, "STS.sk", cred.SecretKey)
	assert.Equal(t, "STS.token", cred.SecurityToken)
	assert.Equal(t, "tos-cn-beijing.volces.com", cred.Endpoint)
	assert.Equal(t, "cn-beijing", cred.Region)
	assert.Equal(t, "photos", cred.Bucket)
	assert.Equal(t, "uploads/", cred.ObjectKeyPrefix)
	expectedExpiry := time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)
	assert.WithinDuration(t, expectedExpiry, cred.ExpiresAt, time.Second)
}

// TestVolcProvider_GetSTSToken_BadExpiration verifies parse failure surfaces.
func TestVolcProvider_GetSTSToken_BadExpiration(t *testing.T) {
	fake := &fakeSTS{
		resp: &assumeRoleResp{
			Expiration: "not-a-date",
		},
	}
	p := newVolcProviderWithFakeSTS(fake, "trn:iam::1:role/r")
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse sts expiration")
}

// TestVolcProvider_GetSTSToken_ContextPropagation verifies the context is
// passed through to the underlying stsCli.assumeRole call — Volcengine SDK
// honors ctx for cancellation / deadlines (AssumeRoleWithContext).
func TestVolcProvider_GetSTSToken_ContextPropagation(t *testing.T) {
	fake := &fakeSTS{
		resp: &assumeRoleResp{Expiration: "2026-06-26T15:30:00Z"},
	}
	p := newVolcProviderWithFakeSTS(fake, "trn:iam::1:role/r")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.GetSTSToken(ctx, &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "p/",
		TTL:       15 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, ctx, fake.gotCtx, "ctx must propagate to stsCli.assumeRole")
}

// --- stsClient tests (HTTP-mocked AssumeRole) ---

// hostFromURL strips the scheme from a URL, returning "host[:port]".
func hostFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Host
}

// TestSTSAssumeRole_Success mocks the Volcengine STS API and verifies the
// response mapping. The Volcengine SDK is RPC-style: it POSTs the AssumeRole
// parameters as a form-encoded request body (Action/Version in the query
// string, all input fields in the body), so the test reads them from
// r.Body.
//
// The Volcengine unmarshal handler splits the JSON body into:
//   - ResponseMetadata → resp.Metadata
//   - Result.Credentials → resp.Credentials (CredentialsForAssumeRoleOutput)
//
// Field names per the v1.2.36 SDK: AccessKeyId, SecretAccessKey (NOT
// AccessKeySecret), SessionToken, ExpiredTime, CurrentTime.
func TestSTSAssumeRole_Success(t *testing.T) {
	var capturedBody url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		capturedBody, err = url.ParseQuery(string(bodyBytes))
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ResponseMetadata": {"RequestId": "req-123"},
			"Result": {
				"Credentials": {
					"AccessKeyId": "STS.ak123",
					"SecretAccessKey": "STS.sk123",
					"SessionToken": "STS.token456",
					"ExpiredTime": "2026-06-26T15:30:00Z"
				}
			}
		}`))
	}))
	defer srv.Close()

	c, err := newSTSClient(&stsClientOpts{
		AccessKeyId:     "ak",
		AccessKeySecret: "sk",
		Region:          "cn-beijing",
		Endpoint:        hostFromURL(t, srv.URL),
		DisableSSL:      true,
	})
	require.NoError(t, err)

	duration := int32(900)
	resp, err := c.assumeRole(context.Background(), &assumeRoleReq{
		RoleTrn:         "trn:iam::1234:role/test",
		RoleSessionName: "owner-100",
		DurationSeconds: &duration,
		Policy: map[string]any{
			"Statement": []map[string]any{{
				"Effect":   "Allow",
				"Action":   []string{"tos:PutObject"},
				"Resource": []string{"trn:tos::1234:bucket/uploads/*"},
			}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "req-123", resp.ResponseId)
	assert.Equal(t, "STS.ak123", resp.AccessKeyId)
	assert.Equal(t, "STS.sk123", resp.AccessKeySecret)
	assert.Equal(t, "STS.token456", resp.SessionToken)
	assert.Equal(t, "2026-06-26T15:30:00Z", resp.Expiration)

	// Policy must be sent as a JSON string with no HTML escaping.
	policyStr := capturedBody.Get("Policy")
	require.NotEmpty(t, policyStr, "Policy must be present in request body")
	assert.Contains(t, policyStr, `"Effect":"Allow"`)
	assert.NotContains(t, policyStr, "<", "policy JSON must not HTML-escape")
	assert.Equal(t, "trn:iam::1234:role/test", capturedBody.Get("RoleTrn"))
	assert.Equal(t, "owner-100", capturedBody.Get("RoleSessionName"))
	assert.Equal(t, "900", capturedBody.Get("DurationSeconds"))
}

// TestSTSAssumeRole_APIError verifies SDK errors get wrapped with a clear prefix.
func TestSTSAssumeRole_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"ResponseMetadata":{"Error":{"Code":"NoPermission","Message":"unauthorized"}}}`))
	}))
	defer srv.Close()

	c, err := newSTSClient(&stsClientOpts{
		AccessKeyId:     "ak",
		AccessKeySecret: "sk",
		Region:          "cn-beijing",
		Endpoint:        hostFromURL(t, srv.URL),
		DisableSSL:      true,
	})
	require.NoError(t, err)

	duration := int32(900)
	_, err = c.assumeRole(context.Background(), &assumeRoleReq{
		RoleTrn:         "trn:iam::1234:role/test",
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
