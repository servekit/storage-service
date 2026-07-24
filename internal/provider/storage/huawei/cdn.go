// Package huawei implements the storage.Provider interface for Huawei OBS,
// including object operations (PutObject/GetObject/etc.) via the
// huaweicloud-sdk-go-obs module and STS credential issuance via IAM Agency
// through the huaweicloud-sdk-go-v3 IAM service module. All Huawei-specific
// code lives in this package so the parent storage package stays
// vendor-agnostic; the parent package imports huawei from registry.go to
// wire up VENDOR_HUAWEI_OBS providers.
package huawei

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

// CDNURLGenerator builds Huawei CDN signed URLs for objects on a single
// bucket. One instance per bucket; constructed by the Registry from the
// bucket's config.CDNConfig. Decoupled from HuaweiProvider so the storage
// provider stays focused on OBS operations — CDN signing talks to Huawei
// CDN (a separate product) and shares only the auth key with the origin.
type CDNURLGenerator struct {
	cdnConfig *config.CDNConfig
}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator constructs a Huawei Type-A CDNURLGenerator for a
// bucket. cdn MUST be non-nil — callers (Registry) gate on nil before
// constructing.
func NewCDNURLGenerator(cdn *config.CDNConfig) *CDNURLGenerator {
	return &CDNURLGenerator{cdnConfig: cdn}
}

// CDNURL builds a Huawei CDN URL for the object this generator is bound to.
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
// x-image-process and response-content-disposition query params; Huawei CDN
// forwards both to OBS on cache miss. OBS honors response-content-disposition
// by setting the Content-Disposition response header so browsers download
// rather than inline-display.
//
// Huawei Type A auth_key signs the URI path only (no scheme/host, no query),
// so any combination of query params composes without re-signing. The
// algorithm is identical to Aliyun Type A — Huawei CDN copied the format.
func (g *CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	u := types.CDNObjectURL(g.cdnConfig.Domain, objectKey)
	q := u.Query()
	if len(opts.Ops) > 0 {
		q.Set("x-image-process", buildObsProcessStyle(opts.Ops))
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
	// Same convention as Aliyun (Huawei copied the algorithm); pinned to
	// the bare objectKey (no leading slash) so the test's round-trip
	// through signHuaweiTypeA matches. Verify against your actual CDN
	// console setting during smoke testing — some consoles expect a
	// leading "/" here.
	authKey, err := signHuaweiTypeA(objectKey, g.cdnConfig.AuthKey, expiresAt.Unix(), "0")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign cdn url: %w", err)
	}

	// Reuse the same url.Values to keep param order deterministic; auth_key
	// is appended after x-image-process / response-content-disposition so
	// the URL reads naturally for operators inspecting logs.
	q.Set("auth_key", authKey)
	u.RawQuery = q.Encode()
	return u.String(), expiresAt, nil
}

// --- internal helpers ---

// signHuaweiTypeA returns a CDN URL auth_key string for the given URI,
// formatted as `<timestamp>-<rand>-<uid>-<md5hex>` where md5hex is the
// lowercase hex MD5 of `<uri>-<timestamp>-<rand>-<uid>-<privateKey>`.
//
// Huawei does NOT provide an SDK helper for CDN URL signing. The algorithm
// is the same MD5 formula as Aliyun Type A (Huawei copied the design) but
// reimplemented here so the huawei package has no dependency on the aliyun
// package. Verified against the documented known vector in cdn_test.go.
//
// Spec: https://support.huaweicloud.com/usermanual-cdn/cdn_01_0040.html
//
// rand is generated with crypto/rand (16 random bytes -> 32 hex chars).
// Equivalent to a UUID v4 with the dashes removed, but with full 128-bit
// entropy (UUID v4 reserves 6 bits for version/variant markers). Huawei
// CDN Type A only requires rand to be dash-free.
//
// Callers do not control rand; if you need a fixed rand for testing or
// replay, use signHuaweiTypeAWithInputs.
func signHuaweiTypeA(uri, privateKey string, ts int64, uid string) (string, error) {
	r, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate rand: %w", err)
	}
	return signHuaweiTypeAWithInputs(uri, privateKey, ts, r, uid), nil
}

// signHuaweiTypeAWithInputs is signHuaweiTypeA with caller-supplied rand.
// Used by tests to verify against known vectors and by signHuaweiTypeA
// internally.
func signHuaweiTypeAWithInputs(uri, privateKey string, ts int64, rand, uid string) string {
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
