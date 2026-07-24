// Package tencent implements the storage.Provider interface for Tencent Cloud
// COS via cos-go-sdk-v5. All methods honor ctx — cancellation and timeout
// signals propagate to COS operations.
//
// CDN URL generation lives in the standalone CDNURLGenerator type (cdn.go) —
// this provider only handles COS operations. STS lives in sts.go and is
// opt-in: p.stsCli is nil when AppID is empty at construction time, in which
// case GetSTSToken returns an explicit "not configured" error.
package tencent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// Compile-time assertion that *TencentProvider satisfies types.Provider.
// Catches drift between the interface and the implementation at build time
// rather than at the registry wiring site.
var _ types.Provider = (*TencentProvider)(nil)

// TencentProvider implements the Provider interface for Tencent Cloud COS via
// cos-go-sdk-v5. The COS client is bucket-bound at construction: cos-go-sdk-v5
// takes a single BucketURL on cos.NewClient and all Object/Bucket methods
// resolve against it. The SDK does NOT honor a per-call bucket override, so
// once constructed this provider is pinned to one bucket.
//
// To prevent silent wrong-bucket writes when a caller passes a different
// bucket name, NewTencentProvider parses the bucket name embedded in the
// endpoint host (when it matches the COS virtual-host pattern
// `<bucket>.cos.<region>.myqcloud.com`) and stores it as boundBucket. Each
// Provider method then validates the passed bucket matches; mismatches fail
// fast with a clear error rather than silently hitting the wrong bucket.
// boundBucket is empty for endpoints that don't match the COS pattern (e.g.
// test mocks on 127.0.0.1, regional endpoints without bucket) — validation
// is skipped in that case, but operators should always pass a bucket-level
// URL in production.
//
// Fields are read by tests in this package (sts_test.go constructs a
// TencentProvider literal with only the STS-relevant fields populated), so
// they stay unexported but readable within the package.
type TencentProvider struct {
	client *cos.Client
	// endpoint is the bare COS regional URL passed to NewTencentProvider
	// (e.g. "https://cos.ap-guangzhou.myqcloud.com"). Stored so GetSTSToken
	// can surface it on the returned STSCredential without re-deriving it.
	endpoint  string
	accessKey string
	secretKey string
	region    string
	// boundBucket is the bucket name parsed from the endpoint host when it
	// matches `<bucket>.cos.<region>.myqcloud.com`. Empty when the endpoint
	// doesn't embed a bucket (test mocks, regional endpoints). When non-empty,
	// Provider methods reject calls whose bucket parameter doesn't match —
	// defense against silent wrong-bucket writes.
	boundBucket string
	// appID is the Tencent Cloud APPID (numeric, e.g. "1250000000"). Used as
	// the account segment in STS policy resource ARNs.
	appID string
	// stsCli is nil when AppID was empty at construction — GetSTSToken then
	// returns an explicit "not configured" error so callers fail fast.
	stsCli getCredentialCaller
}

// NewTencentProvider creates a new TencentProvider with the given credentials.
// region is required for STS scoping (resource ARN); endpoint is the
// cos.<region>.myqcloud.com URL and must include the scheme.
//
// roleARN is INTENTIONALLY REJECTED when non-empty: Tencent CAM STS does NOT
// use a RoleARN (it issues temp credentials directly from policy, not by
// assuming a RAM role). Passing one in indicates operator confusion — fail
// fast with a clear message.
//
// appID is the bare numeric APPID (e.g. "1250000000"). When non-empty, STS
// is enabled (p.stsCli is constructed). When empty, STS returns "not
// configured" — callers must use PresignPutObject (presigned PUT) instead.
func NewTencentProvider(endpoint, accessKey, secretKey, roleARN, region, appID string) (*TencentProvider, error) {
	if roleARN != "" {
		return nil, fmt.Errorf("tencent: role_arn must be empty (Tencent CAM STS does not use roles); got %q", roleARN)
	}

	// cos-go-sdk-v5 takes a *url.URL pointing at the bucket. The SDK composes
	// https://<bucket-appid>.cos.<region>.myqcloud.com from BucketURL on each
	// per-bucket call — we pass endpoint verbatim and the SDK appends the
	// object key under it.
	bucketURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse tencent endpoint %q: %w", endpoint, err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  accessKey,
			SecretKey: secretKey,
		},
	})

	p := &TencentProvider{
		client:      client,
		endpoint:    endpoint,
		accessKey:   accessKey,
		secretKey:   secretKey,
		region:      region,
		appID:       appID,
		boundBucket: parseBoundBucket(endpoint),
	}
	if appID != "" {
		stsCli, err := newSTSClient(&stsClientOpts{
			SecretID:  accessKey,
			SecretKey: secretKey,
			AppID:     appID,
			Region:    region,
			// Host empty -> defaults to sts.tencentcloudapi.com for production.
		})
		if err != nil {
			return nil, fmt.Errorf("create sts client: %w", err)
		}
		p.stsCli = stsCli
	}
	return p, nil
}

// PutObject uploads data to the specified bucket and key. size is forwarded
// to the COS Content-Length header so the SDK doesn't have to buffer the
// reader to compute it.
//
// The bucket parameter is unused on the wire (the COS client is bucket-bound
// at construction); kept for interface compatibility with multi-bucket
// providers.
func (p *TencentProvider) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...types.PutOption) error {
	if err := p.checkBucket(bucket); err != nil {
		return err
	}
	putOpts := types.NewPutOptions(opts...)
	opt := &cos.ObjectPutOptions{
		ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{
			ContentLength: size,
		},
	}
	if putOpts.ContentType != "" {
		opt.ObjectPutHeaderOptions.ContentType = putOpts.ContentType
	}
	if _, err := p.client.Object.Put(ctx, key, reader, opt); err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket and key.
// The caller must close the returned reader.
func (p *TencentProvider) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if err := p.checkBucket(bucket); err != nil {
		return nil, err
	}
	resp, err := p.client.Object.Get(ctx, key, nil)
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return resp.Body, nil
}

// DeleteObject removes an object from the specified bucket and key.
func (p *TencentProvider) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := p.checkBucket(bucket); err != nil {
		return err
	}
	if _, err := p.client.Object.Delete(ctx, key, nil); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// HeadObject retrieves metadata for an object without downloading its body.
// When the object is absent, the wrapped error satisfies
// errors.Is(err, types.ErrObjectNotFound).
//
// cos-go-sdk-v5's Object.Head response does not include the object's ACL, so
// a follow-up Object.GetACL call is made to populate ObjectACL. The upload
// service relies on this field to detect ACL violations on private sessions,
// so the extra round trip is intentional. GetACL failure is best-effort: we
// still return the rest of the metadata with an empty ObjectACL.
func (p *TencentProvider) HeadObject(ctx context.Context, bucket, key string) (*types.ObjectInfo, error) {
	if err := p.checkBucket(bucket); err != nil {
		return nil, err
	}
	resp, err := p.client.Object.Head(ctx, key, nil)
	if err != nil {
		if isTencentNotFound(err) {
			return nil, fmt.Errorf("head object %q: %w", key, types.ErrObjectNotFound)
		}
		return nil, fmt.Errorf("head object %q: %w", key, err)
	}

	info := objectInfoFromHead(key, resp)

	// GetACL is best-effort: if it fails (e.g. permission denied on the ACL
	// subresource), we still return the rest of the metadata with an empty
	// ObjectACL rather than failing the entire HeadObject call.
	aclResp, _, aclErr := p.client.Object.GetACL(ctx, key)
	if aclErr == nil && aclResp != nil {
		info.ObjectACL = tencentACLOwnerPermission(aclResp)
	}

	return info, nil
}

// PresignPutObject generates a presigned URL for uploading an object.
// Signed headers (Content-Type, Cache-Control) are surfaced in the returned
// http.Header so the caller can forward them on the actual PUT request —
// without matching headers the upload fails signature validation.
//
// Tencent COS does not support upload-time image processing (imageMogr2 is
// a GET-only API used for download/transformation). Callers needing
// post-upload processing should use a presigned GET with image ops.
func (p *TencentProvider) PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.PutPresignOption) (string, http.Header, error) {
	if err := p.checkBucket(bucket); err != nil {
		return "", nil, err
	}
	putOpts := types.NewPutPresignOptions(opts...)
	header := http.Header{}
	if putOpts.ContentType != "" {
		header.Set("Content-Type", putOpts.ContentType)
	}
	if putOpts.CacheControl != "" {
		header.Set("Cache-Control", putOpts.CacheControl)
	}
	opt := &cos.PresignedURLOptions{}
	if len(header) > 0 {
		opt.Header = &header
	}

	presignedURL, err := p.client.Object.GetPresignedURL(ctx, http.MethodPut, key, p.accessKey, p.secretKey, ttl, opt)
	if err != nil {
		return "", nil, fmt.Errorf("sign put url for %q: %w", key, err)
	}

	// Surface signed headers so callers can forward them to the client.
	// Without these headers the client's upload fails signature validation.
	var headers http.Header
	if opt.Header != nil && len(*opt.Header) > 0 {
		headers = opt.Header.Clone()
	}
	return presignedURL.String(), headers, nil
}

// PresignGetObject generates a presigned URL for downloading an object.
//
// When WithPublic() is passed, returns an unsigned URL of the form
// https://<bucket>.<endpoint>/<key>. The caller MUST verify the object's
// bucket ACL is "public-read" before requesting this mode — no further
// signing check is done here.
//
// imageMogr2 image ops are added as a query param (opt.Query) which the SDK
// folds into the signed URL. Tencent's presigned URL builder signs all query
// params, so the imageMogr2 value is part of the signature — clients must not
// modify it after receipt.
func (p *TencentProvider) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.GetPresignOption) (string, error) {
	if err := p.checkBucket(bucket); err != nil {
		return "", err
	}
	getOpts := types.NewGetPresignOptions(opts...)
	if getOpts.Public {
		return publicObjectURL(p.endpoint, bucket, key), nil
	}
	query := url.Values{}
	if getOpts.Filename != "" {
		query.Set("response-content-disposition", types.BuildContentDisposition(getOpts.Filename))
	}
	if getOpts.ResponseContentType != "" {
		query.Set("response-content-type", getOpts.ResponseContentType)
	}
	if getOpts.ResponseCacheControl != "" {
		query.Set("response-cache-control", getOpts.ResponseCacheControl)
	}
	if len(getOpts.ImageOps) > 0 {
		query.Set("imageMogr2", buildTencentStyle(getOpts.ImageOps))
	}
	opt := &cos.PresignedURLOptions{
		Query: &query,
	}

	presignedURL, err := p.client.Object.GetPresignedURL(ctx, http.MethodGet, key, p.accessKey, p.secretKey, ttl, opt)
	if err != nil {
		return "", fmt.Errorf("sign get url for %q: %w", key, err)
	}
	return presignedURL.String(), nil
}

// ListObjects lists all objects under the given prefix in the COS bucket bound
// to this provider. Pagination is handled internally — the SDK returns up to
// MaxKeys per call and we follow NextMarker until IsTruncated is false.
func (p *TencentProvider) ListObjects(ctx context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	if err := p.checkBucket(bucket); err != nil {
		return nil, err
	}
	var result []types.ObjectInfo
	var marker string
	for {
		opt := &cos.BucketGetOptions{
			Prefix:  prefix,
			MaxKeys: 1000,
			Marker:  marker,
		}
		resp, _, err := p.client.Bucket.Get(ctx, opt)
		if err != nil {
			return nil, fmt.Errorf("list objects prefix=%q: %w", prefix, err)
		}
		for _, obj := range resp.Contents {
			result = append(result, types.ObjectInfo{
				Key:          obj.Key,
				Size:         obj.Size,
				ETag:         strings.Trim(obj.ETag, `"`),
				LastModified: parseTencentTime(obj.LastModified),
			})
		}
		if !resp.IsTruncated {
			break
		}
		marker = resp.NextMarker
	}

	return result, nil
}

// --- internal helpers ---

// errBucketMismatch is returned by Provider methods when the caller passes a
// bucket that doesn't match the bucket the COS client is bound to. Because
// cos-go-sdk-v5 ignores the bucket parameter and always uses the BucketURL
// set at construction, silently accepting the mismatch would route the
// request to the wrong bucket — wrap with explicit context so callers can
// distinguish from a generic transport error.
func errBucketMismatch(passed, bound string) error {
	return fmt.Errorf("tencent: bucket mismatch: caller passed %q but provider is bound to %q (cos-go-sdk-v5 client is bucket-bound at construction; configure one provider per bucket)", passed, bound)
}

// parseBoundBucket extracts the bucket name from a COS virtual-host URL host.
// Returns "" when the host doesn't match the `<bucket>.cos.<region>.myqcloud.com`
// pattern (e.g. test mocks on 127.0.0.1, or regional endpoints without a
// bucket prefix). Operators SHOULD pass bucket-level URLs in production so
// the bound bucket can be enforced; missing bucket = no enforcement.
func parseBoundBucket(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	host := u.Host
	// COS virtual-host pattern: <bucket-appid>.cos.<region>.myqcloud.com
	// Strip the leading segment before the first dot and verify the rest
	// looks like a COS host.
	idx := strings.Index(host, ".")
	if idx <= 0 {
		return ""
	}
	first := host[:idx]
	rest := host[idx+1:]
	if !strings.HasPrefix(rest, "cos.") || !strings.HasSuffix(rest, ".myqcloud.com") {
		return ""
	}
	return first
}

// checkBucket returns errBucketMismatch if the provider is bound to a
// specific bucket and the caller passed a different one. No-op when
// boundBucket is empty (e.g. test endpoints that don't embed a bucket).
func (p *TencentProvider) checkBucket(bucket string) error {
	if p.boundBucket == "" || bucket == p.boundBucket {
		return nil
	}
	return errBucketMismatch(bucket, p.boundBucket)
}

// objectInfoFromHead translates the cos-go-sdk-v5 Head response into a
// types.ObjectInfo. ObjectACL is left empty here; HeadObject fills it via a
// separate GetACL call. Extracted so the mapping can be unit-tested without
// a live endpoint.
//
// *cos.Response embeds *http.Response, so Header / ContentLength are accessed
// via the promoted fields. ContentLength is int64 (from net/http) — no string
// parsing needed, unlike the field-name-collision case on some other SDKs.
func objectInfoFromHead(key string, head *cos.Response) *types.ObjectInfo {
	info := &types.ObjectInfo{
		Key: key,
	}
	if head == nil || head.Response == nil {
		return info
	}
	info.Size = head.ContentLength
	info.ETag = strings.Trim(head.Header.Get("ETag"), `"`)
	info.ContentType = head.Header.Get("Content-Type")
	if lm := head.Header.Get("Last-Modified"); lm != "" {
		info.LastModified = parseTencentTime(lm)
	}
	return info
}

// isTencentNotFound reports whether err is a Tencent COS "object/bucket
// absent" response. cos-go-sdk-v5 surfaces HTTP errors as *cos.ErrorResponse
// with Response.StatusCode set; we unwrap with errors.As so wrapped errors
// (e.g. retry / context wrappers) still match.
func isTencentNotFound(err error) bool {
	var svcErr *cos.ErrorResponse
	if errors.As(err, &svcErr) {
		return svcErr.Response != nil && svcErr.Response.StatusCode == http.StatusNotFound
	}
	return false
}

// tencentACLOwnerPermission inspects an ACL response and returns the
// canonical ACL string. COS ACL responses contain a list of grants; we map
// any READ grant on a non-owner principal to "public-read" (someone other
// than the owner can read), and the absence of such a grant to "private"
// (only the owner has access).
func tencentACLOwnerPermission(acl *cos.ACLXml) string {
	if acl == nil {
		return ""
	}
	ownerID := ""
	if acl.Owner != nil {
		ownerID = acl.Owner.ID
	}
	for _, grant := range acl.AccessControlList {
		if !strings.EqualFold(grant.Permission, "READ") && !strings.EqualFold(grant.Permission, "FULL_CONTROL") {
			continue
		}
		// A READ grant to a non-owner principal means the object is publicly
		// readable (or at least readable outside the owner). Treat as
		// public-read.
		if grant.Grantee == nil {
			return types.ObjectACLPublicRead
		}
		if grant.Grantee.ID != ownerID {
			return types.ObjectACLPublicRead
		}
	}
	// No external READ grant -> only owner has access -> private.
	return types.ObjectACLPrivate
}

// publicObjectURL builds the unsigned URL for a public-read COS object:
// https://<bucket>.<endpoint>/<key>. The endpoint is normalized so callers
// may pass it with or without a scheme, and with or without a trailing slash.
//
// COS uses virtual-host style for public URLs:
//
//	https://<bucket-appid>.cos.<region>.myqcloud.com/<key>
//
// so the bucket is prepended to the host (not the path).
func publicObjectURL(endpoint, bucket, key string) string {
	ep := endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	ep = strings.TrimSuffix(ep, "/")
	schemeSep := "://"
	if idx := strings.Index(ep, schemeSep); idx >= 0 {
		scheme := ep[:idx+len(schemeSep)]
		host := ep[idx+len(schemeSep):]
		return scheme + bucket + "." + host + "/" + strings.TrimPrefix(key, "/")
	}
	// Fallback: path-style URL. Rarely hit (only when endpoint is non-http).
	return ep + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
}

// parseTencentTime parses a COS-format time string. COS returns ISO8601 with
// timezone (RFC3339) for most calls; some legacy paths return the HTTP date
// format ("Mon, 02 Jan 2006 15:04:05 GMT"). Try RFC3339 first, fall back to
// HTTP date. Zero time on failure (caller checks IsZero).
func parseTencentTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := http.ParseTime(s); err == nil {
		return t
	}
	return time.Time{}
}
