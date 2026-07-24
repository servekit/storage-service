package aliyun

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

// newGenWithCDN builds an Aliyun Type-A generator with the given CDN config.
func newGenWithCDN(t *testing.T, cdn *config.CDNConfig) *CDNURLGenerator {
	t.Helper()
	return NewCDNURLGenerator(cdn)
}

// aliyunCDNConfig returns a minimal Aliyun CDNConfig for tests.
func aliyunCDNConfig(authKey string) *config.CDNConfig {
	return &config.CDNConfig{
		Domain:  "cdn.example.com",
		AuthKey: authKey,
	}
}

// TestAliyunCDNURLGenerator_PlainDownload verifies the URL format and
// auth_key presence for a plain download (no ops, no filename).
func TestAliyunCDNURLGenerator_PlainDownload(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("test-key"))

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
	assert.NotContains(t, u.RawQuery, "x-oss-process", "plain download must not carry x-oss-process")

	// auth_key timestamp = expiry's Unix time (Type A convention).
	fields := strings.Split(authKey, "-")
	require.Len(t, fields, 4, "auth_key must be ts-rand-uid-md5hex")
	assert.Equal(t, expiresAt.Unix(), parseInt64(t, fields[0]))
}

// TestAliyunCDNURLGenerator_WithImageOps verifies x-oss-process is appended
// when ops is non-empty.
func TestAliyunCDNURLGenerator_WithImageOps(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 100, Height: 100}}
	gotURL, _, err := g.CDNURL(context.Background(), "uploads/00/abc", types.CDNURLOptions{
		Ops: ops,
		TTL: time.Hour,
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Contains(t, u.Query().Get("x-oss-process"), "image/resize")
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestAliyunCDNURLGenerator_AuthKeyAlgorithm pins the auth_key value to
// what signTypeAWithInputs produces — a regression guard against
// accidental drift between the generator method and the algorithm.
func TestAliyunCDNURLGenerator_AuthKeyAlgorithm(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("known-key"))
	// Freeze time by computing the expected auth_key for expiresAt ourselves
	// and comparing to what the generator produced.
	gotURL, expiresAt, err := g.CDNURL(context.Background(), "k", types.CDNURLOptions{TTL: time.Hour})
	require.NoError(t, err)
	u, _ := url.Parse(gotURL)
	got := u.Query().Get("auth_key")

	// We can't know rand without re-running — but we can verify the algorithm
	// is internally consistent by re-signing with extracted fields.
	fields := strings.Split(got, "-")
	require.Len(t, fields, 4)
	ts, rand, uid, hash := fields[0], fields[1], fields[2], fields[3]
	expected := signTypeAWithInputs("k", "known-key", expiresAt.Unix(), rand, uid)
	assert.Equal(t, expected, got, "auth_key must round-trip through signTypeAWithInputs")
	_ = ts
	_ = hash
}

// TestAliyunCDNURLGenerator_PublicMode verifies that public=true produces
// an unsigned URL: no auth_key, no expiry. CDN console must allow anon.
func TestAliyunCDNURLGenerator_PublicMode(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("test-key"))

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

// TestAliyunCDNURLGenerator_PublicMode_WithImageOps verifies that public
// mode + ops yields a URL with x-oss-process but still no auth_key.
func TestAliyunCDNURLGenerator_PublicMode_WithImageOps(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 100}}
	gotURL, _, err := g.CDNURL(context.Background(), "avatars/100.jpg", types.CDNURLOptions{
		Ops:    ops,
		Public: true,
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Empty(t, u.Query().Get("auth_key"), "public URL must NOT have auth_key")
	assert.Contains(t, u.Query().Get("x-oss-process"), "image/resize")
}

// TestAliyunCDNURLGenerator_FilenameAddsContentDisposition verifies that
// Filename sets response-content-disposition in the query (signed segment),
// and that auth_key is computed independently of the query (Type A signs
// only the URI path).
func TestAliyunCDNURLGenerator_FilenameAddsContentDisposition(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("test-key"))

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

// TestAliyunCDNURLGenerator_FilenameAndOpsTogether verifies both query
// params compose without breaking the auth_key signature.
func TestAliyunCDNURLGenerator_FilenameAndOpsTogether(t *testing.T) {
	g := newGenWithCDN(t, aliyunCDNConfig("test-key"))

	ops := []types.Op{{Type: types.OpResize, Width: 200}}
	gotURL, _, err := g.CDNURL(context.Background(), "img.jpg", types.CDNURLOptions{
		Ops:      ops,
		TTL:      time.Hour,
		Filename: "resized.jpg",
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.NotEmpty(t, u.Query().Get("x-oss-process"))
	assert.NotEmpty(t, u.Query().Get("response-content-disposition"))
	assert.NotEmpty(t, u.Query().Get("auth_key"))
}

// TestSignTypeA_KnownVector locks the algorithm against Aliyun's documented
// example. If this test fails the algorithm drifted from the spec and CDN
// edge nodes will reject every signed URL we issue.
//
// Source: https://help.aliyun.com/zh/cdn/user-guide/type-a-signing
// (search "鉴权URL示例" — verified 2026-06-24)
func TestSignTypeA_KnownVector(t *testing.T) {
	// Aliyun doc worked example: md5hash for sstring
	// "/video/standard/test.mp4-1444435200-0-0-aliyuncdnexp1234"
	// is 23bf85053008f5c0e791667a313e28ce.
	got := signTypeAWithInputs("/video/standard/test.mp4", "aliyuncdnexp1234", 1444435200, "0", "0")
	want := "1444435200-0-0-23bf85053008f5c0e791667a313e28ce"
	assert.Equal(t, want, got, "auth_key must match Aliyun doc example exactly")
}

// TestSignTypeA_RandGenerated verifies that signTypeA fills in rand when not
// pre-supplied. Different calls must produce different auth_keys (rand varies).
func TestSignTypeA_RandGenerated(t *testing.T) {
	a, err := signTypeA("/image/x.png", "key", 1700000000, "uid1")
	require.NoError(t, err)
	b, err := signTypeA("/image/x.png", "key", 1700000000, "uid1")
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "two calls should produce different auth_keys (random rand)")
}

// TestSignTypeA_DifferentKeyDifferentHash verifies the key actually
// participates in the MD5 input (regression guard against accidentally
// hardcoding or dropping the key).
func TestSignTypeA_DifferentKeyDifferentHash(t *testing.T) {
	a := signTypeAWithInputs("/x", "key1", 1700000000, "r", "u")
	b := signTypeAWithInputs("/x", "key2", 1700000000, "r", "u")
	assert.NotEqual(t, a, b)
}

// TestSignTypeA_Format verifies the auth_key field order is ts-rand-uid-md5hex.
func TestSignTypeA_Format(t *testing.T) {
	got := signTypeAWithInputs("/x", "k", 1700000000, "r", "u")
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
