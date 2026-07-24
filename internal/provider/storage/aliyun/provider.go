// Package aliyun implements the storage.Provider interface for Aliyun OSS,
// including OSS operations (PutObject/GetObject/etc.) and STS credential
// issuance via AssumeRole. All Aliyun-specific code lives in this package
// so the parent storage package stays vendor-agnostic; the parent package
// imports aliyun from registry.go to wire up VENDOR_ALIYUN_OSS providers.
package aliyun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// AliyunProvider implements the Provider interface for Aliyun OSS via the
// v2 SDK (github.com/aliyun/alibabacloud-oss-go-sdk-v2). All methods honor
// ctx — cancellation and timeout signals propagate to OSS operations.
//
// CDN URL generation lives in the standalone CDNURLGenerator type — this
// provider only handles OSS operations.
type AliyunProvider struct {
	client    *oss.Client
	endpoint  string
	accessKey string
	secretKey string
	region    string
	roleARN   string
	stsCli    assumeRoleCaller // nil if RoleARN unconfigured; GetSTSToken returns error
}

// NewAliyunProvider creates a new AliyunProvider with the given credentials.
// region is required by the v2 SDK for request signing; endpoint is optional
// (when empty, v2 derives it from region). roleARN is optional — when
// non-empty, the provider can issue STS credentials via AssumeRole; when
// empty, GetSTSToken returns an explicit error.
func NewAliyunProvider(endpoint, accessKey, secretKey, roleARN, region string) (*AliyunProvider, error) {
	cfg := oss.LoadDefaultConfig().
		WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey)).
		WithRegion(region)
	if endpoint != "" {
		cfg = cfg.WithEndpoint(endpoint)
	}
	client := oss.NewClient(cfg)
	p := &AliyunProvider{
		client:    client,
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		roleARN:   roleARN,
	}
	if roleARN != "" {
		stsCli, err := newSTSClient(&stsClientOpts{
			AccessKeyId:     accessKey,
			AccessKeySecret: secretKey,
			RegionId:        region,
			Endpoint:        stsEndpointFor(region),
			// Protocol empty → SDK defaults to HTTPS for production.
		})
		if err != nil {
			return nil, fmt.Errorf("create sts client: %w", err)
		}
		p.stsCli = stsCli
	}
	return p, nil
}

// PutObject uploads data to the specified bucket and key.
func (p *AliyunProvider) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...types.PutOption) error {
	putOpts := types.NewPutOptions(opts...)
	req := &oss.PutObjectRequest{
		Bucket:        oss.Ptr(bucket),
		Key:           oss.Ptr(key),
		Body:          reader,
		ContentLength: oss.Ptr(size),
	}
	if putOpts.ContentType != "" {
		req.ContentType = oss.Ptr(putOpts.ContentType)
	}
	if _, err := p.client.PutObject(ctx, req); err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket and key.
// The caller must close the returned reader.
func (p *AliyunProvider) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	result, err := p.client.GetObject(ctx, &oss.GetObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return result.Body, nil
}

// DeleteObject removes an object from the specified bucket and key.
func (p *AliyunProvider) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := p.client.DeleteObject(ctx, &oss.DeleteObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(key),
	}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// HeadObject retrieves metadata for an object without downloading its body.
// When the object is absent, the wrapped error satisfies errors.Is(err, types.ErrObjectNotFound).
//
// The v2 SDK's HeadObjectResult does not include the x-oss-object-acl header,
// so a follow-up GetObjectAcl call is made to populate ObjectACL. The upload
// service relies on this field to detect ACL violations on private sessions,
// so the extra round trip is intentional.
func (p *AliyunProvider) HeadObject(ctx context.Context, bucket, key string) (*types.ObjectInfo, error) {
	head, err := p.client.HeadObject(ctx, &oss.HeadObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(key),
	})
	if err != nil {
		if isAliyunNotFound(err) {
			return nil, fmt.Errorf("head object %q: %w", key, types.ErrObjectNotFound)
		}
		return nil, fmt.Errorf("head object %q: %w", key, err)
	}

	info := objectInfoFromHead(key, head)

	// GetObjectAcl is best-effort: if it fails (e.g. permission denied on the
	// ACL subresource), we still return the rest of the metadata with an empty
	// ObjectACL rather than failing the entire HeadObject call.
	aclResp, aclErr := p.client.GetObjectAcl(ctx, &oss.GetObjectAclRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(key),
	})
	if aclErr == nil && aclResp != nil && aclResp.ACL != nil {
		info.ObjectACL = oss.ToString(aclResp.ACL)
	}

	return info, nil
}

// PresignPutObject generates a presigned URL for uploading an object.
// Options signed into the URL require the client to send matching headers.
//
// Aliyun OSS does not support upload-time image processing via presigned PUT
// (x-oss-process is GET-only; upload-time processing requires the separate
// ProcessObject/AsyncProcessObject APIs after upload). Callers needing
// post-upload processing should call those APIs explicitly.
func (p *AliyunProvider) PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.PutPresignOption) (string, http.Header, error) {
	putOpts := types.NewPutPresignOptions(opts...)
	req := &oss.PutObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(key),
	}
	if putOpts.ContentType != "" {
		req.ContentType = oss.Ptr(putOpts.ContentType)
	}
	if putOpts.CacheControl != "" {
		req.CacheControl = oss.Ptr(putOpts.CacheControl)
	}
	if len(putOpts.Metadata) > 0 {
		req.Metadata = putOpts.Metadata
	}

	result, err := p.client.Presign(ctx, req, oss.PresignExpires(ttl))
	if err != nil {
		return "", nil, fmt.Errorf("sign put url for %q: %w", key, err)
	}

	// Surface signed headers so callers can forward them to the client.
	// Without these headers the client's upload fails signature validation.
	var headers http.Header
	if len(result.SignedHeaders) > 0 {
		headers = make(http.Header, len(result.SignedHeaders))
		for k, v := range result.SignedHeaders {
			headers.Set(k, v)
		}
	}
	return result.URL, headers, nil
}

// PresignGetObject generates a presigned URL for downloading an object.
//
// When WithPublic() is passed, returns an unsigned URL of the form
// https://<bucket>.<endpoint>/<key>. The caller MUST verify the object's
// bucket ACL is "public_read" before requesting this mode — no further
// signing check is done here.
func (p *AliyunProvider) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.GetPresignOption) (string, error) {
	getOpts := types.NewGetPresignOptions(opts...)
	if getOpts.Public {
		return publicObjectURL(p.endpoint, bucket, key), nil
	}
	req := &oss.GetObjectRequest{
		Bucket: oss.Ptr(bucket),
		Key:    oss.Ptr(key),
	}
	if getOpts.Filename != "" {
		req.ResponseContentDisposition = oss.Ptr(types.BuildContentDisposition(getOpts.Filename))
	}
	if getOpts.ResponseContentType != "" {
		req.ResponseContentType = oss.Ptr(getOpts.ResponseContentType)
	}
	if getOpts.ResponseCacheControl != "" {
		req.ResponseCacheControl = oss.Ptr(getOpts.ResponseCacheControl)
	}
	if len(getOpts.ImageOps) > 0 {
		req.Process = oss.Ptr(buildOssProcessStyle(getOpts.ImageOps))
	}

	result, err := p.client.Presign(ctx, req, oss.PresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("sign get url for %q: %w", key, err)
	}
	return result.URL, nil
}

// ListObjects lists all objects under the given prefix in the specified bucket.
func (p *AliyunProvider) ListObjects(ctx context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	paginator := p.client.NewListObjectsV2Paginator(&oss.ListObjectsV2Request{
		Bucket: oss.Ptr(bucket),
		Prefix: oss.Ptr(prefix),
	})

	var result []types.ObjectInfo
	for paginator.HasNext() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list objects prefix=%q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			result = append(result, types.ObjectInfo{
				Key:          oss.ToString(obj.Key),
				Size:         obj.Size,
				ETag:         strings.Trim(oss.ToString(obj.ETag), `"`),
				LastModified: oss.ToTime(obj.LastModified),
			})
		}
	}

	return result, nil
}

// --- internal helpers ---

// objectInfoFromHead translates the v2 HeadObjectResult into a types.ObjectInfo.
// ObjectACL is left empty here; HeadObject fills it via a separate GetObjectAcl
// call. Extracted so the mapping can be unit-tested without a live endpoint.
func objectInfoFromHead(key string, head *oss.HeadObjectResult) *types.ObjectInfo {
	info := &types.ObjectInfo{
		Key:         key,
		Size:        head.ContentLength,
		ETag:        strings.Trim(oss.ToString(head.ETag), `"`),
		ContentType: oss.ToString(head.ContentType),
	}
	if head.LastModified != nil {
		info.LastModified = oss.ToTime(head.LastModified)
	}
	return info
}

// isAliyunNotFound reports whether err is an Aliyun OSS "object/bucket absent"
// response. The v2 SDK surfaces 404s as *oss.ServiceError with StatusCode==404
// (Code "NoSuchKey" / "NoSuchBucket"). OperationError wraps transport-level
// errors, so we use errors.As to unwrap.
func isAliyunNotFound(err error) bool {
	var svcErr *oss.ServiceError
	if errors.As(err, &svcErr) {
		return svcErr.StatusCode == http.StatusNotFound
	}
	return false
}

// publicObjectURL builds the unsigned URL for a public-read OSS object:
// https://<bucket>.<endpoint>/<key>. The endpoint is normalized so callers
// may pass it with or without a scheme, and with or without a trailing slash.
// Query escaping is left to the caller's URL client; key bytes are inserted
// verbatim because OSS keys are path segments (slashes preserved).
func publicObjectURL(endpoint, bucket, key string) string {
	ep := endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	ep = strings.TrimSuffix(ep, "/")
	if strings.HasPrefix(ep, "https://") || strings.HasPrefix(ep, "http://") {
		scheme := ep[:strings.Index(ep, "://")+3]
		host := ep[strings.Index(ep, "://")+3:]
		// OSS uses <bucket>.<endpoint> virtual-host style for public URLs
		// (e.g. https://my-bucket.oss-cn-hangzhou.aliyuncs.com/<key>).
		return scheme + bucket + "." + host + "/" + strings.TrimPrefix(key, "/")
	}
	// Fallback: path-style URL. Rarely hit (only when endpoint is non-http).
	return ep + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
}
