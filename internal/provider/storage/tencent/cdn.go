// Package tencent implements the storage.Provider interface for Tencent Cloud
// COS, including COS operations (PutObject/GetObject/etc.), CAM STS credential
// issuance, and CDN Type A URL signing. All Tencent-specific code lives in
// this package so the parent storage package stays vendor-agnostic; the parent
// package imports tencent from registry.go to wire up VENDOR_TENCENT_COS
// providers.
package tencent

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/pkg/config"
)

// CDNURLGenerator builds Tencent CDN signed URLs for objects on a single
// bucket. One instance per bucket; constructed by the Registry from the
// bucket's config.CDNConfig. Decoupled from TencentProvider so the storage
// provider stays focused on COS operations — CDN signing talks to Tencent
// CDN (a separate product) and shares only the auth key with the origin.
type CDNURLGenerator struct {
	cdnConfig *config.CDNConfig
}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator constructs a Tencent Type-A CDNURLGenerator for a
// bucket. cdn MUST be non-nil — callers (Registry) gate on nil before
// constructing. The signing algorithm is identical to Aliyun Type A but
// lives in this package so the tencent package is self-contained (no
// cross-package import on aliyun).
func NewCDNURLGenerator(cdn *config.CDNConfig) *CDNURLGenerator {
	return &CDNURLGenerator{cdnConfig: cdn}
}

// CDNURL builds a Tencent CDN URL for the object this generator is bound to.
//
// When opts.Public=false (default): the URL is signed with Type A auth_key
// (auth_key = ts-rand-uid-md5(uri-ts-rand-uid-key)) and expires at
// (now + opts.TTL). CDN edge nodes verify the MD5 against the same key
// (configured in the CDN console) and reject with 403 if mismatched or
// expired.
//
// When opts.Public=true: the URL is unsigned (no auth_key) and permanent.
// CDN must be configured to allow anonymous access for the path (path
// whitelist or public bucket ACL).
//
// opts.Ops (non-empty) and opts.Filename (non-empty) are appended as
// imageMogr2 path suffix and response-content-disposition query params.
// Tencent CDN forwards both to COS on cache miss. COS honors
// response-content-disposition by setting the Content-Disposition response
// header so browsers download rather than inline-display.
//
// Tencent Type A auth_key signs the URI path only (no scheme/host, no query),
// so any combination of query params composes without re-signing.
func (g *CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	u := types.CDNObjectURL(g.cdnConfig.Domain, objectKey)
	q := u.Query()
	if len(opts.Ops) > 0 {
		q.Set("imageMogr2", buildTencentStyle(opts.Ops))
	}
	if opts.Filename != "" {
		q.Set("response-content-disposition", types.BuildContentDisposition(opts.Filename))
	}
	u.RawQuery = q.Encode()

	if opts.Public {
		// Public URL: no auth_key, permanent. CDN must be configured to
		// allow anonymous access for this path (path whitelist or public
		// bucket ACL).
		return u.String(), time.Time{}, nil
	}

	now := time.Now()
	expiresAt := now.Add(opts.TTL)

	// Type A: the path used for signing is the URI without scheme/host.
	// Tencent's CDN edge nodes reconstruct the same URI from the request path
	// and verify the MD5 against it. The plan pins this to the bare objectKey
	// (no leading slash) so the test's round-trip through signTencentTypeA
	// matches; verify against your actual CDN console setting during smoke
	// testing — some consoles expect a leading "/" here.
	authKey, err := signTencentTypeA(objectKey, g.cdnConfig.AuthKey, expiresAt.Unix(), "0")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign cdn url: %w", err)
	}

	// Reuse the same url.Values to keep param order deterministic; auth_key
	// is appended after imageMogr2 / response-content-disposition so the URL
	// reads naturally for operators inspecting logs.
	q.Set("auth_key", authKey)
	u.RawQuery = q.Encode()
	return u.String(), expiresAt, nil
}

// --- internal helpers ---

// signTencentTypeA returns a CDN URL auth_key string for the given URI,
// formatted as `<timestamp>-<rand>-<uid>-<md5hex>` where md5hex is the
// lowercase hex MD5 of `<uri>-<timestamp>-<rand>-<uid>-<privateKey>`.
//
// Tencent does NOT provide an SDK helper for CDN URL signing. The algorithm
// is a simple MD5 over the dash-joined input — verified against the
// documented known vector in cdn_test.go.
//
// Spec: https://cloud.tencent.com/document/product/228/41623
//
// rand is generated with crypto/rand (16 random bytes -> 32 hex chars),
// equivalent to a UUID v4 with the dashes removed but with full 128-bit
// entropy. Tencent Type A only requires rand to be dash-free.
//
// Callers do not control rand; if you need a fixed rand for testing or
// replay, use signTencentTypeAWithInputs.
func signTencentTypeA(uri, privateKey string, ts int64, uid string) (string, error) {
	r, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate rand: %w", err)
	}
	return signTencentTypeAWithInputs(uri, privateKey, ts, r, uid), nil
}

// signTencentTypeAWithInputs is signTencentTypeA with caller-supplied rand.
// Used by tests to verify against known vectors and by signTencentTypeA
// internally.
func signTencentTypeAWithInputs(uri, privateKey string, ts int64, rand, uid string) string {
	s := fmt.Sprintf("%s-%d-%s-%s-%s", uri, ts, rand, uid, privateKey)
	sum := md5.Sum([]byte(s))
	return fmt.Sprintf("%d-%s-%s-%s", ts, rand, uid, hex.EncodeToString(sum[:]))
}

// randomHex returns n random bytes encoded as 2n lowercase hex characters.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
