package fake

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// CDNURLGenerator is a test-only types.CDNURLGenerator that emits placeholder
// URLs (cdn.test.example host, fake_auth/expires query params). Injected into
// a Registry via NewRegistryWithProvider when service-layer tests need to
// exercise GetCDNURL without depending on a real CDN vendor.
type CDNURLGenerator struct{}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator returns a placeholder CDNURLGenerator for tests.
func NewCDNURLGenerator() *CDNURLGenerator {
	return &CDNURLGenerator{}
}

// CDNURL returns a placeholder CDN URL. Mirrors the real generator contract:
//
// When opts.Public=false: the fake_auth query param carries a deterministic
// signature and expires carries the expiry timestamp, so tests can assert
// on their presence.
//
// When opts.Public=true: no fake_auth or expires query params are added (the
// URL is unsigned and permanent). ops and filename are still reflected as
// x-oss-process / response-content-disposition so tests can verify wiring.
func (*CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	u := &url.URL{
		Scheme: "https",
		Host:   "cdn.test.example",
		Path:   "/" + objectKey,
	}
	q := u.Query()
	if len(opts.Ops) > 0 {
		q.Set("x-oss-process", "fake-style")
	}
	if opts.Filename != "" {
		q.Set("response-content-disposition", types.BuildContentDisposition(opts.Filename))
	}
	u.RawQuery = q.Encode()

	if opts.Public {
		return u.String(), time.Time{}, nil
	}
	expiresAt := time.Now().Add(opts.TTL)
	q.Set("fake_auth", "test-signature")
	q.Set("expires", strconv.FormatInt(expiresAt.Unix(), 10))
	u.RawQuery = q.Encode()
	return u.String(), expiresAt, nil
}
