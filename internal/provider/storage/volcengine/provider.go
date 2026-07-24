// Package volcengine implements the storage.Provider interface for Volcengine
// TOS, including TOS operations (PutObject/GetObject/etc.) and STS credential
// issuance via AssumeRole. All Volcengine-specific code lives in this package
// so the parent storage package stays vendor-agnostic; the parent package
// imports volcengine from registry.go to wire up VENDOR_VOLCENGINE_TOS
// providers.
package volcengine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/volcengine/ve-tos-golang-sdk/v2/tos"
	"github.com/volcengine/ve-tos-golang-sdk/v2/tos/enum"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// VolcengineProvider implements the Provider interface for Volcengine TOS via
// the v2 SDK (github.com/volcengine/ve-tos-golang-sdk/v2). All methods honor
// ctx — cancellation and timeout signals propagate to TOS operations.
//
// CDN URL generation lives in the standalone CDNURLGenerator type — this
// provider only handles TOS operations.
type VolcengineProvider struct {
	client    *tos.ClientV2
	endpoint  string
	accessKey string
	secretKey string
	region    string
	roleTRN   string
	stsCli    assumeRoleCaller // nil if RoleTRN unconfigured; GetSTSToken returns error
}

// Compile-time assertion that *VolcengineProvider satisfies types.Provider.
var _ types.Provider = (*VolcengineProvider)(nil)

// NewVolcengineProvider creates a new VolcengineProvider with the given
// credentials. region is required by the v2 SDK for request signing; endpoint
// is required (TOS native endpoint form, e.g. tos-cn-beijing.volces.com —
// NOT S3-compatible path). roleTRN is optional — when non-empty (TRN format
// "trn:iam::<account-id>:role/<role-name>"), the provider can issue STS
// credentials via AssumeRole; when empty, GetSTSToken returns an explicit
// error.
func NewVolcengineProvider(endpoint, accessKey, secretKey, roleTRN, region string) (*VolcengineProvider, error) {
	client, err := tos.NewClientV2(endpoint, tos.WithRegion(region),
		tos.WithCredentials(tos.NewStaticCredentials(accessKey, secretKey)))
	if err != nil {
		return nil, fmt.Errorf("create tos client: %w", err)
	}
	p := &VolcengineProvider{
		client:    client,
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		roleTRN:   roleTRN,
	}
	if roleTRN != "" {
		stsCli, err := newSTSClient(&stsClientOpts{
			AccessKeyId:     accessKey,
			AccessKeySecret: secretKey,
			Region:          region,
		})
		if err != nil {
			return nil, fmt.Errorf("create sts client: %w", err)
		}
		p.stsCli = stsCli
	}
	return p, nil
}

// PutObject uploads data to the specified bucket and key.
func (p *VolcengineProvider) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...types.PutOption) error {
	putOpts := types.NewPutOptions(opts...)
	req := &tos.PutObjectV2Input{
		Content: reader,
	}
	req.Bucket = bucket
	req.Key = key
	req.ContentLength = size
	if putOpts.ContentType != "" {
		req.ContentType = putOpts.ContentType
	}
	if _, err := p.client.PutObjectV2(ctx, req); err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket and key.
// The caller must close the returned reader.
func (p *VolcengineProvider) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	result, err := p.client.GetObjectV2(ctx, &tos.GetObjectV2Input{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return result.Content, nil
}

// DeleteObject removes an object from the specified bucket and key.
func (p *VolcengineProvider) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := p.client.DeleteObjectV2(ctx, &tos.DeleteObjectV2Input{
		Bucket: bucket,
		Key:    key,
	}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// HeadObject retrieves metadata for an object without downloading its body.
// When the object is absent, the wrapped error satisfies errors.Is(err, types.ErrObjectNotFound).
//
// TOS HeadObjectV2 returns standard object metadata in headers but does NOT
// include the canned ACL string. The ACL is fetched via a follow-up
// GetObjectACL call (ClientV2 method has no V2 suffix) and translated from
// Grants into the canned ACL string the rest of the project expects.
func (p *VolcengineProvider) HeadObject(ctx context.Context, bucket, key string) (*types.ObjectInfo, error) {
	head, err := p.client.HeadObjectV2(ctx, &tos.HeadObjectV2Input{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		if isVolcNotFound(err) {
			return nil, fmt.Errorf("head object %q: %w", key, types.ErrObjectNotFound)
		}
		return nil, fmt.Errorf("head object %q: %w", key, err)
	}

	info := objectInfoFromHead(key, head)

	// GetObjectACL is best-effort: if it fails we still return the rest of
	// the metadata with an empty ObjectACL rather than failing HeadObject.
	aclResp, aclErr := p.client.GetObjectACL(ctx, &tos.GetObjectACLInput{
		Bucket: bucket,
		Key:    key,
	})
	if aclErr == nil && aclResp != nil {
		info.ObjectACL = volcACLFromGrants(aclResp)
	}

	return info, nil
}

// PresignPutObject generates a presigned URL for uploading an object.
// Options signed into the URL require the client to send matching headers.
//
// Volcengine TOS does not support upload-time image processing via presigned
// PUT (x-tos-process is GET-only). Callers needing post-upload processing
// should call the TOS ProcessImage API explicitly.
//
// TOS PreSignedURLInput carries optional headers and query params as maps
// rather than typed fields, so ContentType/CacheControl are placed into the
// Header map. The returned SignedHeader lists what the client must send back.
func (p *VolcengineProvider) PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.PutPresignOption) (string, http.Header, error) {
	putOpts := types.NewPutPresignOptions(opts...)
	req := &tos.PreSignedURLInput{
		HTTPMethod: enum.HttpMethodPut,
		Bucket:     bucket,
		Key:        key,
		Expires:    int64(ttl.Seconds()),
		Header:     map[string]string{},
	}
	if putOpts.ContentType != "" {
		req.Header["Content-Type"] = putOpts.ContentType
	}
	if putOpts.CacheControl != "" {
		req.Header["Cache-Control"] = putOpts.CacheControl
	}

	output, err := p.client.PreSignedURL(req)
	if err != nil {
		return "", nil, fmt.Errorf("sign put url for %q: %w", key, err)
	}

	var headers http.Header
	if len(output.SignedHeader) > 0 {
		headers = make(http.Header, len(output.SignedHeader))
		for k, v := range output.SignedHeader {
			headers.Set(k, v)
		}
	}
	return output.SignedUrl, headers, nil
}

// PresignGetObject generates a presigned URL for downloading an object.
//
// When WithPublic() is passed, returns an unsigned URL of the form
// https://<bucket>.<endpoint>/<key>. The caller MUST verify the object's
// bucket ACL is "public-read" before requesting this mode — no further
// signing check is done here.
//
// Response overrides (Filename, ResponseContentType, ResponseCacheControl)
// and image ops are placed into the Query map because PreSignedURLInput
// surfaces them as generic query params (TOS uses the S3-style
// response-content-disposition / response-content-type / x-tos-process keys).
func (p *VolcengineProvider) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.GetPresignOption) (string, error) {
	getOpts := types.NewGetPresignOptions(opts...)
	if getOpts.Public {
		return publicObjectURL(p.endpoint, bucket, key), nil
	}
	req := &tos.PreSignedURLInput{
		HTTPMethod: enum.HttpMethodGet,
		Bucket:     bucket,
		Key:        key,
		Expires:    int64(ttl.Seconds()),
		Query:      map[string]string{},
	}
	if getOpts.Filename != "" {
		req.Query["response-content-disposition"] = types.BuildContentDisposition(getOpts.Filename)
	}
	if getOpts.ResponseContentType != "" {
		req.Query["response-content-type"] = getOpts.ResponseContentType
	}
	if getOpts.ResponseCacheControl != "" {
		req.Query["response-cache-control"] = getOpts.ResponseCacheControl
	}
	if len(getOpts.ImageOps) > 0 {
		req.Query["x-tos-process"] = buildVolcStyle(getOpts.ImageOps)
	}

	output, err := p.client.PreSignedURL(req)
	if err != nil {
		return "", fmt.Errorf("sign get url for %q: %w", key, err)
	}
	return output.SignedUrl, nil
}

// GetSTSToken retrieves temporary STS credentials via AssumeRole. Requires
// RoleTRN to be configured at NewVolcengineProvider time; otherwise returns
// an explicit error so callers know to use PresignPutObject instead.
func (p *VolcengineProvider) GetSTSToken(ctx context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
	if p == nil || p.stsCli == nil || p.roleTRN == "" {
		return nil, fmt.Errorf("volcengine STS not configured for this provider; set provider.role_arn in config")
	}
	if policy == nil {
		return nil, fmt.Errorf("nil sts policy")
	}

	policyJSON, err := buildVolcPolicy(policy, parseVolcAccount(p.roleTRN))
	if err != nil {
		return nil, fmt.Errorf("build sts policy: %w", err)
	}

	duration := int32(policy.TTL.Seconds())
	if duration <= 0 {
		return nil, fmt.Errorf("sts policy: TTL must be > 0")
	}
	if duration < minVolcSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v below Volcengine AssumeRole minimum of %ds",
			policy.TTL, minVolcSTSDuration)
	}

	// RoleSessionName embeds OwnerID so TOS audit logs can trace credentials
	// back to the originating user. OwnerID is not sensitive.
	resp, err := p.stsCli.assumeRole(ctx, &assumeRoleReq{
		RoleTrn:         p.roleTRN,
		RoleSessionName: fmt.Sprintf("owner-%d", policy.OwnerID),
		DurationSeconds: &duration,
		Policy:          policyJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("volcengine sts assume role: %w", err)
	}

	expiresAt, err := time.Parse(time.RFC3339, resp.Expiration)
	if err != nil {
		return nil, fmt.Errorf("parse sts expiration %q: %w", resp.Expiration, err)
	}

	return &types.STSCredential{
		AccessKey:       resp.AccessKeyId,
		SecretKey:       resp.AccessKeySecret,
		SecurityToken:   resp.SessionToken,
		Endpoint:        p.endpoint,
		Region:          p.region,
		Bucket:          policy.Bucket,
		ObjectKeyPrefix: policy.KeyPrefix,
		ExpiresAt:       expiresAt,
	}, nil
}

// ListObjects lists all objects under the given prefix in the specified bucket.
func (p *VolcengineProvider) ListObjects(ctx context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	var result []types.ObjectInfo
	var continuationToken string
	for {
		req := &tos.ListObjectsType2Input{
			Bucket:            bucket,
			Prefix:            prefix,
			ContinuationToken: continuationToken,
		}
		page, err := p.client.ListObjectsType2(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("list objects prefix=%q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			result = append(result, types.ObjectInfo{
				Key:          obj.Key,
				Size:         obj.Size,
				ETag:         strings.Trim(obj.ETag, `"`),
				LastModified: obj.LastModified,
			})
		}
		if !page.IsTruncated {
			break
		}
		continuationToken = page.NextContinuationToken
	}

	return result, nil
}

// --- internal helpers ---

// objectInfoFromHead translates the v2 HeadObjectV2Output into a
// types.ObjectInfo. ObjectACL is left empty here; HeadObject fills it via a
// separate GetObjectACL call. Extracted so the mapping can be unit-tested
// without a live endpoint.
//
// Note: TOS ObjectMetaV2 exposes the field as ETag (capital E), unlike OSS.
func objectInfoFromHead(key string, head *tos.HeadObjectV2Output) *types.ObjectInfo {
	info := &types.ObjectInfo{
		Key:         key,
		Size:        head.ContentLength,
		ETag:        strings.Trim(head.ETag, `"`),
		ContentType: head.ContentType,
	}
	if !head.LastModified.IsZero() {
		info.LastModified = head.LastModified
	}
	return info
}

// isVolcNotFound reports whether err is a Volcengine TOS 404 response.
//
// TOS surfaces unexpected status codes (including 404) as
// *tos.UnexpectedStatusCodeError, which carries StatusCode directly. Some
// paths surface *tos.TosServerError instead — that type has no StatusCode
// field, so we use the SDK's tos.StatusCode helper which inspects both.
func isVolcNotFound(err error) bool {
	if err == nil {
		return false
	}
	return tos.StatusCode(err) == http.StatusNotFound
}

// volcACLFromGrants translates the Grants list returned by GetObjectACL into
// the canned ACL string the rest of the project expects ("private",
// "public-read", "public-read-write"). TOS does not surface a canned ACL
// string directly — when IsDefault is true the object inherits the bucket
// ACL and we surface "default"; otherwise we inspect Grants for the
// AllUsers grantee permission.
func volcACLFromGrants(resp *tos.GetObjectACLOutput) string {
	if resp == nil {
		return ""
	}
	if resp.IsDefault {
		return "default"
	}
	allUsersRead := false
	allUsersWrite := false
	for _, g := range resp.Grants {
		// TOS uses Canned == "AllUsers" (enum.CannedAllUsers) for the
		// anonymous group, S3-equivalent of the AllUsers URI grantee.
		if g.GranteeV2.Canned == enum.CannedAllUsers {
			switch g.Permission {
			case enum.PermissionRead:
				allUsersRead = true
			case enum.PermissionWrite:
				allUsersWrite = true
			case enum.PermissionFullControl:
				allUsersRead = true
				allUsersWrite = true
			}
		}
	}
	switch {
	case allUsersRead && allUsersWrite:
		return "public-read-write"
	case allUsersRead:
		return "public-read"
	default:
		return "private"
	}
}

// publicObjectURL builds the unsigned URL for a public-read TOS object:
// https://<bucket>.<endpoint>/<key>. Endpoint is normalized so callers may
// pass it with or without a scheme, and with or without a trailing slash.
func publicObjectURL(endpoint, bucket, key string) string {
	ep := endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	ep = strings.TrimSuffix(ep, "/")
	if strings.HasPrefix(ep, "https://") || strings.HasPrefix(ep, "http://") {
		scheme := ep[:strings.Index(ep, "://")+3]
		host := ep[strings.Index(ep, "://")+3:]
		// TOS supports <bucket>.<endpoint> virtual-host style for public URLs.
		return scheme + bucket + "." + host + "/" + strings.TrimPrefix(key, "/")
	}
	return ep + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
}
