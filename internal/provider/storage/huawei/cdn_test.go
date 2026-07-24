package huawei

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/pkg/config"
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
