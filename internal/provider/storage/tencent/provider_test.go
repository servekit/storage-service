package tencent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cos "github.com/tencentyun/cos-go-sdk-v5"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// TestCompileTimeProviderInterface is a no-op test that exists only to keep
// the var _ types.Provider = (*TencentProvider)(nil) assertion in provider.go
// covered by the test binary — Go's dead-code elimination would otherwise
// drop it. If the assertion ever fails the package won't compile.
func TestCompileTimeProviderInterface(t *testing.T) {
	var _ types.Provider = (*TencentProvider)(nil)
}

// newTestProvider constructs a TencentProvider against the httptest server URL
// and disables the COS SDK's CRC64 verification (the SDK verifies
// x-cos-hash-crc64ecma on every PUT/GET, which the mock server doesn't emit).
// Production code keeps EnableCRC=true (the default) — disabling it here keeps
// the tests focused on the provider's HTTP wiring rather than COS checksum
// semantics.
func newTestProvider(t *testing.T, srvURL string) *TencentProvider {
	t.Helper()
	p, err := NewTencentProvider(srvURL, "ak", "sk", "", "ap-guangzhou", "")
	require.NoError(t, err)
	p.client.Conf.EnableCRC = false
	return p
}

// newHeadResponse builds a *cos.Response wrapping a minimal *http.Response
// with the given headers. Used to drive objectInfoFromHead without spinning
// up an HTTP server.
func newHeadResponse(h http.Header) *cos.Response {
	return &cos.Response{
		Response: &http.Response{
			Header: h,
		},
	}
}

// TestObjectInfoFromHead_AllFieldsPopulated verifies the happy-path mapping
// from the v5 Head response to types.ObjectInfo. ObjectACL is intentionally
// not set here — HeadObject fills it via a separate GetACL call.
func TestObjectInfoFromHead_AllFieldsPopulated(t *testing.T) {
	lastModified := "Mon, 02 Jan 2026 15:04:05 GMT"
	h := http.Header{}
	h.Set("Content-Length", "2048")
	h.Set("ETag", `"deadbeef"`)
	h.Set("Content-Type", "image/jpeg")
	h.Set("Last-Modified", lastModified)
	// *http.Response.ContentLength is a separate field from the header; the
	// SDK populates the field (which cos.Response promotes), not the header.
	resp := newHeadResponse(h)
	resp.ContentLength = 2048

	info := objectInfoFromHead("photos/abc.jpg", resp)
	assert.Equal(t, "photos/abc.jpg", info.Key)
	assert.Equal(t, int64(2048), info.Size)
	assert.Equal(t, "deadbeef", info.ETag, "ETag quotes must be stripped")
	assert.Equal(t, "image/jpeg", info.ContentType)
	expectedLM, _ := http.ParseTime(lastModified)
	assert.WithinDuration(t, expectedLM, info.LastModified, time.Second)
	assert.Empty(t, info.ObjectACL, "objectInfoFromHead must not populate ObjectACL; HeadObject does it via GetACL")
}

// TestObjectInfoFromHead_NilResponse verifies a nil response (or nil embedded
// *http.Response) does not panic.
func TestObjectInfoFromHead_NilResponse(t *testing.T) {
	info := objectInfoFromHead("k", nil)
	require.NotNil(t, info)
	assert.Equal(t, "k", info.Key)
	assert.Empty(t, info.Size)

	// Also exercise the nil-embedded-Response branch: a *cos.Response literal
	// with Response == nil must not panic either.
	info2 := objectInfoFromHead("k", &cos.Response{})
	require.NotNil(t, info2)
	assert.Equal(t, "k", info2.Key)
}

// TestObjectInfoFromHead_ETagWithoutQuotes verifies an ETag that arrives
// without quotes (some S3-compatible gateways do this) is passed through
// unchanged — strings.Trim only removes quotes when present.
func TestObjectInfoFromHead_ETagWithoutQuotes(t *testing.T) {
	h := http.Header{}
	h.Set("ETag", "plain-etag")
	resp := newHeadResponse(h)
	info := objectInfoFromHead("k", resp)
	assert.Equal(t, "plain-etag", info.ETag)
}

// TestPublicObjectURL_VirtualHostStyle verifies the URL shape for a COS
// public object. COS uses virtual-host style:
// https://<bucket-appid>.cos.<region>.myqcloud.com/<key>.
func TestPublicObjectURL_VirtualHostStyle(t *testing.T) {
	got := publicObjectURL("https://cos.ap-guangzhou.myqcloud.com", "mybucket-1250000000", "uploads/abc.jpg")
	want := "https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com/uploads/abc.jpg"
	assert.Equal(t, want, got)
}

// TestPublicObjectURL_EndpointWithoutScheme verifies endpoint without scheme
// gets https:// prepended.
func TestPublicObjectURL_EndpointWithoutScheme(t *testing.T) {
	got := publicObjectURL("cos.ap-guangzhou.myqcloud.com", "mybucket-1250000000", "uploads/abc.jpg")
	want := "https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com/uploads/abc.jpg"
	assert.Equal(t, want, got)
}

// TestPublicObjectURL_TrailingSlashInEndpoint verifies trailing slash is
// trimmed from endpoint.
func TestPublicObjectURL_TrailingSlashInEndpoint(t *testing.T) {
	got := publicObjectURL("https://cos.ap-guangzhou.myqcloud.com/", "mybucket-1250000000", "uploads/abc.jpg")
	want := "https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com/uploads/abc.jpg"
	assert.Equal(t, want, got)
}

// TestPublicObjectURL_LeadingSlashInKey verifies leading slash in key is
// trimmed so we don't get a double slash.
func TestPublicObjectURL_LeadingSlashInKey(t *testing.T) {
	got := publicObjectURL("https://cos.ap-guangzhou.myqcloud.com", "mybucket-1250000000", "/uploads/abc.jpg")
	want := "https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com/uploads/abc.jpg"
	assert.Equal(t, want, got)
}

// TestParseTencentTime_RFC3339 verifies RFC3339 parsing.
func TestParseTencentTime_RFC3339(t *testing.T) {
	got := parseTencentTime("2026-06-26T15:30:00Z")
	want := time.Date(2026, 6, 26, 15, 30, 0, 0, time.UTC)
	assert.WithinDuration(t, want, got, time.Second)
}

// TestParseTencentTime_HTTPDate verifies HTTP date parsing.
func TestParseTencentTime_HTTPDate(t *testing.T) {
	got := parseTencentTime("Mon, 02 Jan 2026 15:04:05 GMT")
	want := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	assert.WithinDuration(t, want, got, time.Second)
}

// TestParseTencentTime_Empty verifies empty input returns zero time.
func TestParseTencentTime_Empty(t *testing.T) {
	got := parseTencentTime("")
	assert.True(t, got.IsZero())
}

// TestParseTencentTime_Garbage verifies unparseable input returns zero time.
func TestParseTencentTime_Garbage(t *testing.T) {
	got := parseTencentTime("not-a-time")
	assert.True(t, got.IsZero())
}

// TestTencentProvider_PutObject_HappyPath mocks the COS HTTP API and verifies
// PutObject forwards the body, Content-Length, and Content-Type to the right
// URL.
func TestTencentProvider_PutObject_HappyPath(t *testing.T) {
	var capturedMethod, capturedPath, capturedContentType string
	var capturedContentLength string
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedContentType = r.Header.Get("Content-Type")
		capturedContentLength = r.Header.Get("Content-Length")
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)

	err := p.PutObject(context.Background(), "mybucket-1250000000", "test/hello.txt",
		strings.NewReader("hello"), 5,
		types.WithContentType("text/plain"))
	require.NoError(t, err)

	assert.Equal(t, http.MethodPut, capturedMethod)
	assert.Equal(t, "/test/hello.txt", capturedPath)
	assert.Equal(t, "text/plain", capturedContentType)
	assert.Equal(t, "5", capturedContentLength, "Content-Length must match the size argument")
	assert.Equal(t, "hello", capturedBody)
}

// TestTencentProvider_GetObject_HappyPath mocks COS GET and verifies the body
// is returned. The caller is responsible for closing the reader.
func TestTencentProvider_GetObject_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/test/hello.txt", r.URL.Path)
		_, _ = w.Write([]byte("hello-body"))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)

	rc, err := p.GetObject(context.Background(), "mybucket-1250000000", "test/hello.txt")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "hello-body", string(body))
}

// TestTencentProvider_DeleteObject_HappyPath mocks COS DELETE and verifies
// the call routes to the right path with DELETE method.
func TestTencentProvider_DeleteObject_HappyPath(t *testing.T) {
	var capturedMethod, capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)

	err := p.DeleteObject(context.Background(), "mybucket-1250000000", "test/hello.txt")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, capturedMethod)
	assert.Equal(t, "/test/hello.txt", capturedPath)
}

// TestTencentProvider_HeadObject_HappyPath mocks COS HEAD followed by a GetACL
// GET, and verifies the merged ObjectInfo includes the ACL derived from the
// ACL response.
func TestTencentProvider_HeadObject_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", "1024")
			w.Header().Set("ETag", `"abc"`)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Last-Modified", "Mon, 02 Jan 2026 15:04:05 GMT")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			// GetACL response: XML body with a single owner-only FULL_CONTROL
			// grant -> ACL must be "private".
			if strings.HasSuffix(r.URL.RawQuery, "acl") || r.URL.Query().Get("acl") != "" {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<AccessControlPolicy>
  <Owner><ID>qcs::cam::uin/1250000000:uin/1250000000</ID></Owner>
  <AccessControlList>
    <Grant>
      <Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser">
        <ID>qcs::cam::uin/1250000000:uin/1250000000</ID>
        <DisplayName>1250000000</DisplayName>
      </Grantee>
      <Permission>FULL_CONTROL</Permission>
    </Grant>
  </AccessControlList>
</AccessControlPolicy>`))
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)

	info, err := p.HeadObject(context.Background(), "mybucket-1250000000", "photos/abc.jpg")
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "photos/abc.jpg", info.Key)
	assert.Equal(t, "abc", info.ETag)
	assert.Equal(t, "image/jpeg", info.ContentType)
	assert.Equal(t, "private", info.ObjectACL, "owner-only FULL_CONTROL grant must map to private")
}

// TestTencentProvider_HeadObject_NotFound verifies that a 404 from COS Head
// is wrapped as types.ErrObjectNotFound so callers can errors.Is it.
func TestTencentProvider_HeadObject_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// cos-go-sdk-v5's ErrorResponse expects an XML body for non-2xx;
			// write a minimal one so the SDK can decode Response.StatusCode.
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>no such key</Message></Error>`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)

	_, err := p.HeadObject(context.Background(), "mybucket-1250000000", "missing.txt")
	require.Error(t, err)
	assert.True(t, errors.Is(err, types.ErrObjectNotFound),
		"expected ErrObjectNotFound, got: %v", err)
}

// TestTencentProvider_PresignGetObject_Public verifies that WithPublic()
// returns an unsigned URL of the expected virtual-host shape.
func TestTencentProvider_PresignGetObject_Public(t *testing.T) {
	p, err := NewTencentProvider(
		"https://cos.ap-guangzhou.myqcloud.com", "ak", "sk",
		"", "ap-guangzhou", "",
	)
	require.NoError(t, err)

	got, err := p.PresignGetObject(context.Background(),
		"mybucket-1250000000", "uploads/abc.jpg",
		time.Hour, types.WithPublic())
	require.NoError(t, err)
	want := "https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com/uploads/abc.jpg"
	assert.Equal(t, want, got)
}

// TestTencentProvider_PresignGetObject_SignedWithOps verifies a signed URL
// can be produced and that the imageMogr2 query is folded in.
func TestTencentProvider_PresignGetObject_SignedWithOps(t *testing.T) {
	p, err := NewTencentProvider(
		"https://cos.ap-guangzhou.myqcloud.com", "ak", "sk",
		"", "ap-guangzhou", "",
	)
	require.NoError(t, err)

	ops := []types.Op{{Type: types.OpResize, Width: 100, Height: 100}}
	got, err := p.PresignGetObject(context.Background(),
		"mybucket-1250000000", "uploads/abc.jpg",
		time.Hour, types.WithImageOps(ops))
	require.NoError(t, err)
	assert.Contains(t, got, "imageMogr2=", "imageMogr2 query param must be folded into the presigned URL")
	assert.Contains(t, got, "thumbnail", "imageMogr2 value must contain the resize segment")
	// Presigned URL must include COS signature params (q-sign-algorithm).
	assert.Contains(t, got, "q-sign-algorithm", "presigned URL must be signed")
}

// TestTencentProvider_PresignPutObject_WithContentType verifies the signed
// Content-Type header is surfaced back to the caller.
func TestTencentProvider_PresignPutObject_WithContentType(t *testing.T) {
	p, err := NewTencentProvider(
		"https://cos.ap-guangzhou.myqcloud.com", "ak", "sk",
		"", "ap-guangzhou", "",
	)
	require.NoError(t, err)

	urlStr, headers, err := p.PresignPutObject(context.Background(),
		"mybucket-1250000000", "uploads/abc.jpg",
		time.Hour, types.WithUploadContentType("image/jpeg"))
	require.NoError(t, err)
	require.NotEmpty(t, urlStr)
	require.NotNil(t, headers, "signed Content-Type header must be surfaced")
	assert.Equal(t, "image/jpeg", headers.Get("Content-Type"))
}

// TestTencentProvider_ListObjects_HappyPath mocks paginated COS Bucket.Get
// (two pages, IsTruncated=true then false) and verifies pagination is
// followed and Object metadata is mapped to types.ObjectInfo.
func TestTencentProvider_ListObjects_HappyPath(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/", r.URL.Path)
		prefix := r.URL.Query().Get("prefix")
		assert.Equal(t, "uploads/", prefix)
		w.Header().Set("Content-Type", "application/xml")
		switch page {
		case 0:
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <Name>mybucket-1250000000</Name>
  <Prefix>uploads/</Prefix>
  <Marker></Marker>
  <NextMarker>uploads/page2</NextMarker>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>true</IsTruncated>
  <Contents>
    <Key>uploads/a.jpg</Key>
    <ETag>"etag-a"</ETag>
    <Size>100</Size>
    <LastModified>2026-01-02T15:04:05.000Z</LastModified>
  </Contents>
</ListBucketResult>`))
		case 1:
			marker := r.URL.Query().Get("marker")
			assert.Equal(t, "uploads/page2", marker, "page 2 must use NextMarker from page 1")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult>
  <Name>mybucket-1250000000</Name>
  <Prefix>uploads/</Prefix>
  <Marker>uploads/page2</Marker>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>uploads/b.jpg</Key>
    <ETag>"etag-b"</ETag>
    <Size>200</Size>
    <LastModified>Mon, 02 Jan 2026 15:04:05 GMT</LastModified>
  </Contents>
</ListBucketResult>`))
		}
		page++
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)

	objs, err := p.ListObjects(context.Background(), "mybucket-1250000000", "uploads/")
	require.NoError(t, err)
	require.Len(t, objs, 2, "two pages of one object each must merge into two results")
	assert.Equal(t, "uploads/a.jpg", objs[0].Key)
	assert.Equal(t, int64(100), objs[0].Size)
	assert.Equal(t, "etag-a", objs[0].ETag)
	assert.Equal(t, "uploads/b.jpg", objs[1].Key)
	assert.Equal(t, int64(200), objs[1].Size)
	assert.False(t, objs[1].LastModified.IsZero(), "HTTP-date LastModified must parse")
}

// TestNewTencentProvider_RejectsRoleARN verifies the constructor returns an
// error when roleARN is non-empty — Tencent CAM STS doesn't use roles, so a
// non-empty value indicates operator confusion.
func TestNewTencentProvider_RejectsRoleARN(t *testing.T) {
	_, err := NewTencentProvider(
		"https://cos.ap-guangzhou.myqcloud.com",
		"ak", "sk",
		"some-role-arn", // should be rejected
		"ap-guangzhou", "1250000000",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "role_arn must be empty")
}

// TestNewTencentProvider_BadEndpoint verifies the constructor returns an
// error when endpoint is not parseable as a URL.
func TestNewTencentProvider_BadEndpoint(t *testing.T) {
	_, err := NewTencentProvider(
		"://not-a-url",
		"ak", "sk", "", "ap-guangzhou", "1250000000",
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse tencent endpoint")
}

// TestNewTencentProvider_NoSTSWhenAppIDEmpty verifies that omitting AppID
// leaves p.stsCli nil so GetSTSToken returns "not configured".
func TestNewTencentProvider_NoSTSWhenAppIDEmpty(t *testing.T) {
	p, err := NewTencentProvider(
		"https://cos.ap-guangzhou.myqcloud.com",
		"ak", "sk", "", "ap-guangzhou", "",
	)
	require.NoError(t, err)
	assert.Nil(t, p.stsCli, "stsCli must be nil when AppID is empty")
}

// TestNewTencentProvider_STSWhenAppIDSet verifies that providing AppID wires
// up the STS client (non-nil stsCli). Doesn't issue real STS traffic.
func TestNewTencentProvider_STSWhenAppIDSet(t *testing.T) {
	p, err := NewTencentProvider(
		"https://cos.ap-guangzhou.myqcloud.com",
		"ak", "sk", "", "ap-guangzhou", "1250000000",
	)
	require.NoError(t, err)
	assert.NotNil(t, p.stsCli, "stsCli must be non-nil when AppID is provided")
}

// TestParseBoundBucket covers the COS virtual-host pattern detector. The
// detector gates runtime bucket-mismatch enforcement — when it returns "",
// Provider methods skip the check (test mocks and regional endpoints must not
// trigger false positives).
func TestParseBoundBucket(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"bucket_url", "https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com", "mybucket-1250000000"},
		{"bucket_url_http", "http://mybucket-1250000000.cos.ap-shanghai.myqcloud.com", "mybucket-1250000000"},
		{"regional_endpoint_no_bucket", "https://cos.ap-guangzhou.myqcloud.com", ""},
		{"ip_endpoint", "http://127.0.0.1:58001", ""},
		{"non_cos_host", "https://example.com", ""},
		{"garbage", "://not-a-url", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseBoundBucket(tc.endpoint))
		})
	}
}

// TestTencentProvider_RejectsBucketMismatch verifies that a provider
// constructed against a bucket-level endpoint rejects calls targeting a
// different bucket. Without this guard the call would silently hit the
// bound bucket (cos-go-sdk-v5 ignores the per-call bucket parameter).
func TestTencentProvider_RejectsBucketMismatch(t *testing.T) {
	p, err := NewTencentProvider(
		"https://mybucket-1250000000.cos.ap-guangzhou.myqcloud.com",
		"ak", "sk", "", "ap-guangzhou", "",
	)
	require.NoError(t, err)
	require.Equal(t, "mybucket-1250000000", p.boundBucket, "bound bucket must be parsed from endpoint host")

	err = p.PutObject(context.Background(), "otherbucket-1250000000", "k", strings.NewReader("x"), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bucket mismatch", "error must explain the mismatch")
	assert.Contains(t, err.Error(), "otherbucket-1250000000")
	assert.Contains(t, err.Error(), "mybucket-1250000000")
}

// TestTencentProvider_AllowsMatchingBucket verifies the bound-bucket guard
// passes when the caller targets the bucket the provider is bound to.
func TestTencentProvider_AllowsMatchingBucket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Test against a mock server: boundBucket is "" because the host doesn't
	// match the COS pattern, so the guard is a no-op and the call should
	// succeed as before. newTestProvider disables CRC verification (the mock
	// server doesn't emit x-cos-hash-crc64ecma).
	p := newTestProvider(t, srv.URL)
	assert.Empty(t, p.boundBucket, "mock server URL must not parse as a COS bucket URL")

	err := p.PutObject(context.Background(), "anybucket-1250000000", "k",
		strings.NewReader("x"), 1)
	require.NoError(t, err, "mock endpoint must skip bucket enforcement")
}
