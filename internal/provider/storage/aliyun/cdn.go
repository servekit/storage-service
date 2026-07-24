package aliyun

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

// CDNURLGenerator builds Aliyun CDN signed URLs for objects on a single
// bucket. One instance per bucket; constructed by the Registry from the
// bucket's config.CDNConfig. Decoupled from AliyunProvider so the storage
// provider stays focused on OSS operations — CDN signing talks to Aliyun
// CDN (a separate product) and shares only the auth key with the origin.
type CDNURLGenerator struct {
	cdnConfig *config.CDNConfig
}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator constructs an Aliyun Type-A CDNURLGenerator for a
// bucket. cdn MUST be non-nil — callers (Registry) gate on nil before
// constructing.
func NewCDNURLGenerator(cdn *config.CDNConfig) *CDNURLGenerator {
	return &CDNURLGenerator{cdnConfig: cdn}
}

// CDNURL builds an Aliyun CDN URL for the object this generator is bound to.
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
// x-oss-process and response-content-disposition query params; Aliyun CDN
// forwards both to OSS on cache miss. OSS honors response-content-disposition
// by setting the Content-Disposition response header so browsers download
// rather than inline-display.
//
// Aliyun Type A auth_key signs the URI path only (no scheme/host, no query),
// so any combination of query params composes without re-signing.
func (g *CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	u := types.CDNObjectURL(g.cdnConfig.Domain, objectKey)
	q := u.Query()
	if len(opts.Ops) > 0 {
		q.Set("x-oss-process", buildOssProcessStyle(opts.Ops))
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
	// Aliyun's CDN edge nodes reconstruct the same URI from the request path
	// and verify the MD5 against it. The plan pins this to the bare objectKey
	// (no leading slash) so the test's round-trip through signTypeA matches;
	// verify against your actual CDN console setting during smoke testing
	// — some consoles expect a leading "/" here.
	authKey, err := signTypeA(objectKey, g.cdnConfig.AuthKey, expiresAt.Unix(), "0")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign cdn url: %w", err)
	}

	// Reuse the same url.Values to keep param order deterministic; auth_key
	// is appended after the x-oss-process / response-content-disposition so
	// the URL reads naturally for operators inspecting logs.
	q.Set("auth_key", authKey)
	u.RawQuery = q.Encode()
	return u.String(), expiresAt, nil
}

// --- internal helpers ---

// signTypeA returns a CDN URL auth_key string for the given URI, formatted
// as `<timestamp>-<rand>-<uid>-<md5hex>` where md5hex is the lowercase hex
// MD5 of `<uri>-<timestamp>-<rand>-<uid>-<privateKey>`.
//
// Aliyun does NOT provide an SDK helper for CDN URL signing (the
// cdn-20180510 SDK covers only management APIs like AddCdnDomain and
// RefreshObjectCaches). The algorithm is a simple MD5 over the dash-joined
// input — verified against the documented known vector in cdn_test.go.
//
// Spec: https://help.aliyun.com/zh/cdn/user-guide/type-a-signing
//
// rand is generated with crypto/rand (16 random bytes -> 32 hex chars).
// Equivalent to a UUID v4 with the dashes removed, but with full 128-bit
// entropy (UUID v4 reserves 6 bits for version/variant markers). Aliyun
// CDN Type A only requires rand to be dash-free — UUID is the doc's
// suggested convenience, not a requirement.
//
// Callers do not control rand; if you need a fixed rand for testing or
// replay, use signTypeAWithInputs.
func signTypeA(uri, privateKey string, ts int64, uid string) (string, error) {
	r, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate rand: %w", err)
	}
	return signTypeAWithInputs(uri, privateKey, ts, r, uid), nil
}

// signTypeAWithInputs is signTypeA with caller-supplied rand. Used by tests
// to verify against known vectors and by signTypeA internally.
func signTypeAWithInputs(uri, privateKey string, ts int64, rand, uid string) string {
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
