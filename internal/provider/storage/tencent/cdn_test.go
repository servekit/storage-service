package tencent

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/pkg/config"
)

// newGenWithCDN builds a Tencent Type-A generator with the given CDN config.
func newGenWithCDN(t *testing.T, cdn *config.CDNConfig) *CDNURLGenerator {
	t.Helper()
	return NewCDNURLGenerator(cdn)
}

// tencentCDNConfig returns a minimal Tencent CDNConfig for tests.
func tencentCDNConfig(authKey string) *config.CDNConfig {
	return &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: authKey,
	}
}

// TestTencentCDNURLGenerator_PlainDownload verifies the URL format and
// auth_key presence for a plain download (no ops, no filename).
func TestTencentCDNURLGenerator_PlainDownload(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("test-key"))

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
	assert.NotContains(t, u.RawQuery, "imageMogr2", "plain download must not carry imageMogr2")

	// auth_key timestamp = expiry's Unix time (Type A convention).
	fields := strings.Split(authKey, "-")
	require.Len(t, fields, 4, "auth_key must be ts-rand-uid-md5hex")
	tsUnix, err := strconv.ParseInt(fields[0], 10, 64)
	require.NoError(t, err, "auth_key ts field must be a decimal int")
	assert.Equal(t, expiresAt.Unix(), tsUnix)
}

// TestTencentCDNURLGenerator_WithImageOps verifies imageMogr2 query param is
// appended when ops is non-empty. Signed alongside the URL.
func TestTencentCDNURLGenerator_WithImageOps(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 100, Height: 100}}
	gotURL, _, err := g.CDNURL(context.Background(), "uploads/00/abc", types.CDNURLOptions{
		Ops: ops,
		TTL: time.Hour,
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Contains(t, u.Query().Get("imageMogr2"), "thumbnail")
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestTencentCDNURLGenerator_AuthKeyAlgorithm pins the auth_key value to
// what signTencentTypeAWithInputs produces — a regression guard against
// accidental drift between the generator method and the algorithm.
func TestTencentCDNURLGenerator_AuthKeyAlgorithm(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("known-key"))
	// Freeze time by computing the expected auth_key for expiresAt ourselves
	// and comparing to what the generator produced.
	gotURL, expiresAt, err := g.CDNURL(context.Background(), "k", types.CDNURLOptions{TTL: time.Hour})
	require.NoError(t, err)
	u, _ := url.Parse(gotURL)
	authKey := u.Query().Get("auth_key")

	// Parse the auth_key fields and verify each part of the format.
	fields := strings.Split(authKey, "-")
	require.Len(t, fields, 4, "auth_key must have 4 dash-separated fields")
	ts, randStr, uid, hash := fields[0], fields[1], fields[2], fields[3]

	// ts must equal expiresAt.Unix(); hash must be 32 lowercase hex chars.
	assert.Equal(t, strconv.FormatInt(expiresAt.Unix(), 10), ts, "ts must match expiresAt")
	assert.Regexp(t, `^[0-9a-f]{32}$`, hash, "hash must be 32 lowercase hex chars")

	// Round-trip: feed the parsed rand and uid back through the algorithm
	// with the same inputs to verify the generator doesn't diverge from the
	// algorithm.
	expected := signTencentTypeAWithInputs("k", "known-key", expiresAt.Unix(), randStr, uid)
	assert.Equal(t, expected, authKey, "auth_key must round-trip through signTencentTypeAWithInputs")
}

// TestTencentCDNURLGenerator_PublicMode verifies that public=true produces
// an unsigned URL: no auth_key, no expiry. CDN console must allow anon.
func TestTencentCDNURLGenerator_PublicMode(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("test-key"))

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

// TestTencentCDNURLGenerator_PublicMode_WithImageOps verifies that public
// mode + ops yields a URL with imageMogr2 but still no auth_key.
func TestTencentCDNURLGenerator_PublicMode_WithImageOps(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 100}}
	gotURL, _, err := g.CDNURL(context.Background(), "avatars/100.jpg", types.CDNURLOptions{
		Ops:    ops,
		Public: true,
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Empty(t, u.Query().Get("auth_key"), "public URL must NOT have auth_key")
	assert.Contains(t, u.Query().Get("imageMogr2"), "thumbnail")
}

// TestTencentCDNURLGenerator_FilenameAddsContentDisposition verifies that
// Filename sets response-content-disposition in the query (signed segment),
// and that auth_key is computed independently of the query (Type A signs
// only the URI path).
func TestTencentCDNURLGenerator_FilenameAddsContentDisposition(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("test-key"))

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

// TestTencentCDNURLGenerator_FilenameAndOpsTogether verifies both query
// params compose without breaking the auth_key signature.
func TestTencentCDNURLGenerator_FilenameAndOpsTogether(t *testing.T) {
	g := newGenWithCDN(t, tencentCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 200}}
	gotURL, _, err := g.CDNURL(context.Background(), "img.jpg", types.CDNURLOptions{
		Ops:      ops,
		TTL:      time.Hour,
		Filename: "resized.jpg",
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.NotEmpty(t, u.Query().Get("imageMogr2"))
	assert.NotEmpty(t, u.Query().Get("response-content-disposition"))
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestSignTencentTypeA_KnownVector locks the algorithm against Tencent's
// documented example. If this test fails the algorithm drifted from the spec
// and CDN edge nodes will reject every signed URL we issue.
//
// Tencent Type A and Aliyun Type A use the same MD5 formula; we reuse the
// Aliyun doc's fixed input as a cross-vendor sanity check (Tencent's own doc
// at cloud.tencent.com/document/product/228/41623 uses an equivalent
// algorithm without publishing a numeric hash in the page text, so we pin to
// the Aliyun example which is byte-identical input/output).
//
// Source: https://help.aliyun.com/zh/cdn/user-guide/type-a-signing
// Source: https://cloud.tencent.com/document/product/228/41623
func TestSignTencentTypeA_KnownVector(t *testing.T) {
	// Documented example: md5hash for sstring
	// "/video/standard/test.mp4-1444435200-0-0-aliyuncdnexp1234"
	// is 23bf85053008f5c0e791667a313e28ce.
	got := signTencentTypeAWithInputs("/video/standard/test.mp4", "aliyuncdnexp1234", 1444435200, "0", "0")
	want := "1444435200-0-0-23bf85053008f5c0e791667a313e28ce"
	assert.Equal(t, want, got, "auth_key must match documented Type A example exactly")
}

// TestSignTencentTypeA_RandGenerated verifies that signTencentTypeA fills in
// rand when not pre-supplied. Different calls must produce different
// auth_keys (rand varies).
func TestSignTencentTypeA_RandGenerated(t *testing.T) {
	a, err := signTencentTypeA("/image/x.png", "key", 1700000000, "uid1")
	require.NoError(t, err)
	b, err := signTencentTypeA("/image/x.png", "key", 1700000000, "uid1")
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "two calls should produce different auth_keys (random rand)")
}

// TestSignTencentTypeA_DifferentKeyDifferentHash verifies the key actually
// participates in the MD5 input (regression guard against accidentally
// hardcoding or dropping the key).
func TestSignTencentTypeA_DifferentKeyDifferentHash(t *testing.T) {
	a := signTencentTypeAWithInputs("/x", "key1", 1700000000, "r", "u")
	b := signTencentTypeAWithInputs("/x", "key2", 1700000000, "r", "u")
	assert.NotEqual(t, a, b)
}

// TestSignTencentTypeA_Format verifies the auth_key field order is
// ts-rand-uid-md5hex.
func TestSignTencentTypeA_Format(t *testing.T) {
	got := signTencentTypeAWithInputs("/x", "k", 1700000000, "r", "u")
	// Pattern: digits-dash-string-dash-string-dash-32hex
	assert.Regexp(t, `^1700000000-r-u-[0-9a-f]{32}$`, got)
}
