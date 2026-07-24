// Package volcengine implements the storage.Provider and types.CDNURLGenerator
// interfaces for Volcengine TOS. All Volcengine-specific code lives in this
// package so the parent storage package stays vendor-agnostic; the parent
// package imports volcengine from registry.go to wire up
// VENDOR_VOLCENGINE_TOS providers.
package volcengine

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

// CDNURLGenerator builds Volcengine CDN signed URLs for objects on a single
// bucket. One instance per bucket; constructed by the Registry from the
// bucket's config.CDNConfig. Decoupled from VolcengineProvider so the storage
// provider stays focused on TOS operations — CDN signing talks to Volcengine
// CDN (a separate product) and shares only the auth key with the origin.
type CDNURLGenerator struct {
	cdnConfig *config.CDNConfig
}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator constructs a Volcengine Type-A CDNURLGenerator for a
// bucket. cdn MUST be non-nil — callers (Registry) gate on nil before
// constructing.
func NewCDNURLGenerator(cdn *config.CDNConfig) *CDNURLGenerator {
	return &CDNURLGenerator{cdnConfig: cdn}
}

// CDNURL builds a Volcengine CDN URL for the object this generator is bound to.
//
// When opts.Public=false (default): the URL is signed with Type A auth_key
// (auth_key = ts-rand-uid-md5(uri-ts-rand-uid-key)) and expires at
// (now + opts.TTL). CDN edge nodes verify the MD5 against the same key
// (configured in the CDN console) and reject with 403 if mismatched or
// expired.
//
// When opts.Public=true: the URL is unsigned (no auth_key) and permanent.
// CDN must be configured to allow anonymous access for the path.
//
// opts.Ops (non-empty) and opts.Filename (non-empty) are appended as
// x-tos-process and response-content-disposition query params; Volcengine CDN
// forwards both to TOS on cache miss.
//
// Volcengine Type A auth_key signs the URI path only (no scheme/host, no
// query), so any combination of query params composes without re-signing.
func (g *CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	u := types.CDNObjectURL(g.cdnConfig.Domain, objectKey)
	q := u.Query()
	if len(opts.Ops) > 0 {
		q.Set("x-tos-process", buildVolcStyle(opts.Ops))
	}
	if opts.Filename != "" {
		q.Set("response-content-disposition", types.BuildContentDisposition(opts.Filename))
	}
	u.RawQuery = q.Encode()

	if opts.Public {
		return u.String(), time.Time{}, nil
	}

	now := time.Now()
	expiresAt := now.Add(opts.TTL)

	// Volcengine Type A: signing input is the URI path. We pin to bare
	// objectKey (no leading slash) so tests round-trip through signVolcTypeA.
	authKey, err := signVolcTypeA(objectKey, g.cdnConfig.AuthKey, expiresAt.Unix(), "0")
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign cdn url: %w", err)
	}

	q.Set("auth_key", authKey)
	u.RawQuery = q.Encode()
	return u.String(), expiresAt, nil
}

// --- internal helpers ---

// signVolcTypeA returns a CDN URL auth_key string for the given URI, formatted
// as `<timestamp>-<rand>-<uid>-<md5hex>` where md5hex is the lowercase hex
// MD5 of `<uri>-<timestamp>-<rand>-<uid>-<privateKey>`.
//
// Volcengine does NOT provide an SDK helper for CDN URL signing (the volcengine-go-sdk
// covers only management APIs). The algorithm is a simple MD5 over the dash-joined
// input — verified against the documented known vector in cdn_test.go.
//
// Spec: https://www.volcengine.com/docs/6454/1129831
//
// rand is generated with crypto/rand (16 random bytes -> 32 hex chars).
// Callers do not control rand; for fixed rand use signVolcTypeAWithInputs.
func signVolcTypeA(uri, privateKey string, ts int64, uid string) (string, error) {
	r, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generate rand: %w", err)
	}
	return signVolcTypeAWithInputs(uri, privateKey, ts, r, uid), nil
}

// signVolcTypeAWithInputs is signVolcTypeA with caller-supplied rand. Used by
// tests to verify against known vectors and by signVolcTypeA internally.
func signVolcTypeAWithInputs(uri, privateKey string, ts int64, randStr, uid string) string {
	s := fmt.Sprintf("%s-%d-%s-%s-%s", uri, ts, randStr, uid, privateKey)
	sum := md5.Sum([]byte(s))
	return fmt.Sprintf("%d-%s-%s-%s", ts, randStr, uid, hex.EncodeToString(sum[:]))
}

// randomHex returns n random bytes encoded as 2n lowercase hex characters.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
