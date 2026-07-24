package huawei

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// newTestProvider constructs a HuaweiProvider pointed at the httptest server.
// The OBS SDK accepts a custom endpoint via the constructor's endpoint
// argument; the test server URL passes through unchanged. roleARN is empty
// so GetSTSToken returns "not configured" — STS paths are exercised in
// TestHuaweiProvider_GetSTSToken_* via newHuaweiProviderWithFakeSTS.
func newTestProvider(t *testing.T, srvURL string) *HuaweiProvider {
	t.Helper()
	p, err := NewHuaweiProvider(srvURL, "ak", "sk", "", "", "cn-north-4")
	require.NoError(t, err)
	return p
}

// newHuaweiProviderWithFakeSTS builds a HuaweiProvider and overrides the STS
// client with a fake so GetSTSToken can be exercised without spinning up an
// IAM HTTP endpoint. p.stsCli is assigned after construction; roleARN is set
// so the GetSTSToken "not configured" guard passes.
func newHuaweiProviderWithFakeSTS(t *testing.T, fake assumeAgencyCaller) *HuaweiProvider {
	t.Helper()
	p, err := NewHuaweiProvider("https://obs.example.com", "ak", "sk",
		"demo-agency", "demo-domain", "cn-north-4")
	require.NoError(t, err)
	p.stsCli = fake
	return p
}

// TestObjectInfoFromHead_AllFieldsPopulated verifies the happy-path mapping
// from OBS GetObjectMetadataOutput to types.ObjectInfo.
func TestObjectInfoFromHead_AllFieldsPopulated(t *testing.T) {
	lastModified := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	// GetObjectMetadataOutput embeds ContentType via two anonymous structs
	// (HttpHeader.ContentType), so we build it then assign by name.
	head := &obs.GetObjectMetadataOutput{
		ContentLength: 2048,
		ETag:          `"deadbeef"`,
		LastModified:  lastModified,
	}
	head.ContentType = "image/jpeg"
	info := objectInfoFromHead("photos/abc.jpg", head)
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

// TestHuaweiACLMapping covers the grant-list → canonical-name helper. Uses
// real obs.Grant / obs.Grantee so a rename in the SDK surfaces here.
func TestHuaweiACLMapping(t *testing.T) {
	// No grants → private.
	assert.Equal(t, types.ObjectACLPrivate, huaweiACLOrPrivate(nil))

	// AllUsers + READ → public-read.
	public := []obs.Grant{{
		Grantee:    obs.Grantee{URI: obs.GroupAllUsers},
		Permission: obs.PermissionRead,
	}}
	assert.Equal(t, types.ObjectACLPublicRead, huaweiACLOrPrivate(public))

	// AllUsers + WRITE (not READ) → private.
	writeOnly := []obs.Grant{{
		Grantee:    obs.Grantee{URI: obs.GroupAllUsers},
		Permission: obs.PermissionWrite,
	}}
	assert.Equal(t, types.ObjectACLPrivate, huaweiACLOrPrivate(writeOnly))

	// A non-AllUsers grant does not elevate to public-read.
	otherGrantee := []obs.Grant{{
		Grantee:    obs.Grantee{ID: "some-user-id"},
		Permission: obs.PermissionRead,
	}}
	assert.Equal(t, types.ObjectACLPrivate, huaweiACLOrPrivate(otherGrantee))
}

// TestHuaweiProvider_PutObject_HappyPath mocks OBS and verifies the request
// URL + headers reach the wire correctly.
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

// TestHuaweiProvider_PutObject_APIError verifies OBS errors are wrapped with
// the operation context.
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

// TestHuaweiProvider_GetObject_HappyPath verifies the body reader is returned
// and the caller can drain it.
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

// TestHuaweiProvider_PresignPutObject_ReturnsURL verifies the presigned PUT
// URL is non-empty, contains the bucket as a virtual-hosted subdomain, and
// surfaces the signed Content-Type header for the client. OBS presigning
// produces a virtual-hosted-style URL (https://<bucket>.<endpoint>/<key>).
func TestHuaweiProvider_PresignPutObject_ReturnsURL(t *testing.T) {
	// Presign is computed locally (no HTTP round trip), so no httptest.Server.
	p := newTestProvider(t, "https://obs.example.com")
	url, headers, err := p.PresignPutObject(context.Background(), "b", "k", time.Hour,
		types.WithUploadContentType("image/jpeg"))
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	// OBS uses virtual-hosted style: https://b.obs.example.com[:443]/k?...
	assert.Contains(t, url, "://b.obs.example.com")
	assert.Contains(t, url, "/k?")
	require.NotNil(t, headers)
	assert.Equal(t, "image/jpeg", headers.Get("Content-Type"))
}

// TestHuaweiProvider_PresignPutObject_WithMetadata verifies user-defined
// metadata is signed as x-obs-meta-<key> headers.
func TestHuaweiProvider_PresignPutObject_WithMetadata(t *testing.T) {
	p := newTestProvider(t, "https://obs.example.com")
	url, headers, err := p.PresignPutObject(context.Background(), "b", "k", time.Hour,
		types.WithUploadMetadata(map[string]string{"author": "john"}))
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	require.NotNil(t, headers)
	// Metadata is surfaced with x-obs-meta- prefix on Huawei (vs x-oss-meta-
	// on Aliyun, x-amz-meta- on S3).
	assert.Equal(t, "john", headers.Get("x-obs-meta-author"))
}

// TestHuaweiProvider_PresignGetObject_Public verifies WithPublic returns the
// unsigned URL and skips the OBS SDK call entirely.
func TestHuaweiProvider_PresignGetObject_Public(t *testing.T) {
	// Public path doesn't hit OBS — no httptest.Server needed.
	p, err := NewHuaweiProvider("obs.example.com", "ak", "sk", "", "", "cn-north-4")
	require.NoError(t, err)

	url, err := p.PresignGetObject(context.Background(), "b", "k", time.Hour, types.WithPublic())
	require.NoError(t, err)
	assert.Equal(t, "https://b.obs.example.com/k", url)
}

// TestHuaweiProvider_PresignGetObject_Signed verifies the signed URL host and
// that the response-content-disposition query param is appended. OBS uses
// virtual-hosted-style URLs (https://<bucket>.<endpoint>/<key>).
func TestHuaweiProvider_PresignGetObject_Signed(t *testing.T) {
	p := newTestProvider(t, "https://obs.example.com")
	url, err := p.PresignGetObject(context.Background(), "b", "k", time.Hour,
		types.WithDownloadFilename("report.pdf"))
	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Contains(t, url, "://b.obs.example.com")
	assert.Contains(t, url, "/k?")
	assert.Contains(t, url, "response-content-disposition=", "filename must be signed into the URL")
}

// TestHuaweiProvider_ListObjects_HappyPath verifies pagination is followed
// (two pages with marker continuation). OBS exposes pagination via
// IsTruncated + NextMarker on the ListObjectsOutput.
func TestHuaweiProvider_ListObjects_HappyPath(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/xml")
		// OBS uses the marker query param (S3-compatible); the first page
		// has no marker, subsequent pages echo back the NextMarker we sent.
		if callCount == 1 {
			// Page 1: 1 object, IsTruncated=true, NextMarker=uploads/m2
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

// TestHuaweiProvider_GetSTSToken_NotConfigured verifies that when roleARN is
// empty (no agency configured), GetSTSToken returns the explicit
// "not configured" error rather than nil-dereferencing the STS client.
func TestHuaweiProvider_GetSTSToken_NotConfigured(t *testing.T) {
	p := newTestProvider(t, "https://obs.example.com") // roleARN empty
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket: "b", KeyPrefix: "uploads/", TTL: 30 * time.Minute,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

// TestHuaweiProvider_GetSTSToken_NilPolicy verifies the nil-policy guard.
func TestHuaweiProvider_GetSTSToken_NilPolicy(t *testing.T) {
	p := newHuaweiProviderWithFakeSTS(t, &fakeSTS{})
	_, err := p.GetSTSToken(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil sts policy")
}

// TestHuaweiProvider_GetSTSToken_BelowMinTTL verifies the DurationSeconds
// lower-bound guard fires locally instead of forwarding to IAM.
func TestHuaweiProvider_GetSTSToken_BelowMinTTL(t *testing.T) {
	p := newHuaweiProviderWithFakeSTS(t, &fakeSTS{})
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "uploads/",
		TTL:       time.Duration(minHuaweiSTSDuration-1) * time.Second,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "below Huawei minimum")
}

// TestHuaweiProvider_GetSTSToken_AboveMaxTTL verifies the DurationSeconds
// upper-bound guard fires locally.
func TestHuaweiProvider_GetSTSToken_AboveMaxTTL(t *testing.T) {
	p := newHuaweiProviderWithFakeSTS(t, &fakeSTS{})
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:    "b",
		KeyPrefix: "uploads/",
		TTL:       time.Duration(maxHuaweiSTSDuration+1) * time.Second,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "above Huawei maximum")
}

// TestHuaweiProvider_GetSTSToken_HappyPath verifies a successful STS issuance
// returns a fully-populated STSCredential with the provider's endpoint and
// region filled in.
func TestHuaweiProvider_GetSTSToken_HappyPath(t *testing.T) {
	wantExpiry := time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)
	fake := &fakeSTS{
		resp: &assumeAgencyResp{
			AccessKey:     "tmp-ak",
			SecretKey:     "tmp-sk",
			SecurityToken: "tmp-token",
			ExpiresAt:     wantExpiry,
		},
	}
	p := newHuaweiProviderWithFakeSTS(t, fake)

	cred, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		OwnerID:   42,
		Bucket:    "photos",
		KeyPrefix: "uploads/",
		TTL:       time.Hour,
	})
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, "tmp-ak", cred.AccessKey)
	assert.Equal(t, "tmp-sk", cred.SecretKey)
	assert.Equal(t, "tmp-token", cred.SecurityToken)
	assert.Equal(t, "photos", cred.Bucket)
	assert.Equal(t, "uploads/", cred.ObjectKeyPrefix)
	assert.Equal(t, "https://obs.example.com", cred.Endpoint)
	assert.Equal(t, "cn-north-4", cred.Region)
	assert.WithinDuration(t, wantExpiry, cred.ExpiresAt, time.Second)

	// Verify the assumeAgency request was built correctly: AgencyName from
	// roleARN, DomainID from domainID, RoleSessionName carries OwnerID,
	// DurationSeconds from TTL (3600s, within [900, 43200]).
	require.NotNil(t, fake.gotReq)
	assert.Equal(t, "demo-agency", fake.gotReq.AgencyName)
	assert.Equal(t, "demo-domain", fake.gotReq.DomainID)
	assert.Equal(t, "owner-42", fake.gotReq.RoleSessionName)
	assert.Equal(t, int32(3600), fake.gotReq.DurationSeconds)
}

// TestHuaweiProvider_GetSTSToken_PolicyBuildError verifies a malformed policy
// (extension missing '.' prefix) surfaces as an error before the IAM call.
func TestHuaweiProvider_GetSTSToken_PolicyBuildError(t *testing.T) {
	p := newHuaweiProviderWithFakeSTS(t, &fakeSTS{})
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket:            "b",
		KeyPrefix:         "uploads/",
		AllowedExtensions: []string{"jpg"}, // missing '.'
		TTL:               time.Hour,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must start with '.'")
}

// TestHuaweiProvider_GetSTSToken_AssumeAgencyError verifies IAM backend errors
// are wrapped with operation context.
func TestHuaweiProvider_GetSTSToken_AssumeAgencyError(t *testing.T) {
	fake := &fakeSTS{err: assertAnError("iam backend down")}
	p := newHuaweiProviderWithFakeSTS(t, fake)
	_, err := p.GetSTSToken(context.Background(), &types.STSPolicy{
		Bucket: "b", KeyPrefix: "uploads/", TTL: time.Hour,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iam create temp access key by agency")
	assert.Contains(t, err.Error(), "iam backend down")
}

// --- internal helpers ---

// assertAnError is a tiny helper that returns an error with the given
// message; used so fakeSTS.err has a non-nil error without dragging another
// test dependency.
func assertAnError(msg string) error { return &simpleError{msg: msg} }

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }
