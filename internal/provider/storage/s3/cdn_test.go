package s3

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/pkg/config"
)

// writeTestPEM generates an RSA private key and writes it to a temp file
// in PEM format. Returns the file path.
func writeTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})

	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(path, pemBytes, 0o600))
	return path
}

// newGenWithCDN builds a CloudFront generator with the given CDN config.
func newGenWithCDN(t *testing.T, cdn *config.CDNConfig) *CDNURLGenerator {
	t.Helper()
	return NewCDNURLGenerator(cdn)
}

// cloudfrontCDNConfig returns a valid CloudFront CDNConfig for tests.
func cloudfrontCDNConfig(t *testing.T) *config.CDNConfig {
	t.Helper()
	return &config.CDNConfig{
		Domain:    "cdn.example.com",
		AuthKey:   writeTestPEM(t),
		KeyPairID: "K2A1B2C3D4E5F6",
	}
}

// TestS3CDNURLGenerator_PlainDownload verifies CloudFront Signed URL format.
func TestS3CDNURLGenerator_PlainDownload(t *testing.T) {
	g := newGenWithCDN(t, cloudfrontCDNConfig(t))

	ttl := time.Hour
	gotURL, expiresAt, err := g.CDNURL(context.Background(), "uploads/00/abc", types.CDNURLOptions{TTL: ttl})
	require.NoError(t, err)

	assert.WithinDuration(t, time.Now().Add(ttl), expiresAt, time.Second)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "cdn.example.com", u.Host)
	assert.Equal(t, "/uploads/00/abc", u.Path)
	// CloudFront Canned Policy (Sign) uses 3 query params: Expires, Signature,
	// Key-Pair-Id. (Policy is only emitted by SignWithPolicy.)
	assert.NotEmpty(t, u.Query().Get("Signature"))
	assert.NotEmpty(t, u.Query().Get("Key-Pair-Id"))
	assert.NotEmpty(t, u.Query().Get("Expires"))
}

// TestS3CDNURLGenerator_ImageOpsRejected verifies S3+CloudFront doesn't
// support image processing at the CDN layer.
func TestS3CDNURLGenerator_ImageOpsRejected(t *testing.T) {
	g := newGenWithCDN(t, cloudfrontCDNConfig(t))

	ops := []types.Op{{Type: types.OpResize, Width: 100}}
	_, _, err := g.CDNURL(context.Background(), "key", types.CDNURLOptions{
		Ops: ops,
		TTL: time.Hour,
	})
	require.ErrorIs(t, err, types.ErrCDNImageProcessingUnsupported)
}

// TestS3CDNURLGenerator_PublicMode verifies that public=true produces an
// unsigned URL: no Signature/Expires/Key-Pair-Id, no expiry. CloudFront
// distribution must allow anonymous access for the path.
func TestS3CDNURLGenerator_PublicMode(t *testing.T) {
	g := newGenWithCDN(t, cloudfrontCDNConfig(t))

	gotURL, expiresAt, err := g.CDNURL(context.Background(), "avatars/100.jpg", types.CDNURLOptions{Public: true})
	require.NoError(t, err)

	assert.True(t, expiresAt.IsZero(), "public URL has no expiry")

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "cdn.example.com", u.Host)
	assert.Equal(t, "/avatars/100.jpg", u.Path)
	assert.Empty(t, u.Query().Get("Signature"), "public URL must NOT have Signature")
	assert.Empty(t, u.Query().Get("Expires"), "public URL must NOT have Expires")
	assert.Empty(t, u.Query().Get("Key-Pair-Id"), "public URL must NOT have Key-Pair-Id")
	assert.Empty(t, u.RawQuery, "public URL must have empty query string")
}

// TestS3CDNURLGenerator_FilenameAddsContentDisposition verifies that
// Filename adds response-content-disposition to the query — and that it
// carries through into the signed URL (Sign signs whatever query is on the
// URL when Sign is called).
func TestS3CDNURLGenerator_FilenameAddsContentDisposition(t *testing.T) {
	g := newGenWithCDN(t, cloudfrontCDNConfig(t))

	gotURL, _, err := g.CDNURL(context.Background(), "report.pdf", types.CDNURLOptions{
		TTL:      time.Hour,
		Filename: "年报.pdf",
	})
	require.NoError(t, err)

	u, err := url.Parse(gotURL)
	require.NoError(t, err)
	cd := u.Query().Get("response-content-disposition")
	require.NotEmpty(t, cd, "response-content-disposition must be set when Filename is non-empty")
	assert.Contains(t, cd, "attachment", "must force download")
	assert.Contains(t, cd, "UTF-8''", "non-ASCII filename must use RFC 5987 filename*")
	// Signature still present → query was signed in.
	assert.NotEmpty(t, u.Query().Get("Signature"))
}
