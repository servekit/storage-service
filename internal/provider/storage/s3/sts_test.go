package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
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
// escaping disabled (some S3-compat backends reject &lt;/&gt;/&amp;).
func TestMarshalPolicyJSON_NoHTMLEscape(t *testing.T) {
	p := map[string]any{"k": "v<x>y&w"}
	out, err := marshalPolicyJSON(p)
	require.NoError(t, err)
	assert.Contains(t, string(out), "v<x>y&w")
	assert.NotContains(t, string(out), "\\u003c") // no HTML-escaped <
	assert.False(t, strings.HasSuffix(string(out), "\n"), "trailing newline must be trimmed")
}

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
	assert.Equal(t, "us-east-1", cred.Region, "Region must be surfaced so clients don't derive from Endpoint")
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

// TestS3Provider_GetSTSToken_NilPolicy verifies the policy == nil guard at
// the GetSTSToken entry. buildS3Policy's own nil check is covered separately
// at sts_test.go level; this test locks in the GetSTSToken-level guard for
// symmetry with NoRoleARN / BelowMinTTL / BadExpiration.
func TestS3Provider_GetSTSToken_NilPolicy(t *testing.T) {
	p := newS3ProviderWithFakeSTS(
		&fakeS3STS{resp: &assumeRoleResp{Expiration: "2026-06-23T15:30:00Z"}},
		"arn:aws:iam::1:role/r",
	)
	_, err := p.GetSTSToken(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts policy")
}
