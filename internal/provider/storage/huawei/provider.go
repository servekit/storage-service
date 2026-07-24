// Package huawei implements the storage.Provider interface for Huawei OBS,
// including OBS operations (PutObject/GetObject/etc.) and STS credential
// issuance via IAM Agency (CreateTemporaryAccessKeyByAgency). All
// Huawei-specific code lives in this package so the parent storage package
// stays vendor-agnostic; the parent package imports huawei from registry.go
// to wire up VENDOR_HUAWEI_OBS providers.
package huawei

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// HuaweiProvider implements the Provider interface for Huawei OBS via the
// huaweicloud-sdk-go-obs module. All methods honor ctx — OBS SDK operations
// do not accept a context in v3.26.3, so cancellation/timeout currently
// cannot propagate into in-flight OBS calls; callers needing hard timeouts
// must rely on the OBS client's HTTP-level configuration.
//
// STS (GetSTSToken) uses a separate IAM-v3 client (委托 Agency) wired in at
// construction time when roleARN (agency name) is non-empty.
//
// CDN URL generation lives in the standalone CDNURLGenerator type — this
// provider only handles OBS operations.
type HuaweiProvider struct {
	client    *obs.ObsClient
	endpoint  string
	accessKey string
	secretKey string
	region    string
	domainID  string             // required by IAM global credentials
	roleARN   string             // agency name; empty = STS unavailable
	stsCli    assumeAgencyCaller // nil when roleARN unconfigured
}

// Compile-time assertion that *HuaweiProvider satisfies types.Provider.
var _ types.Provider = (*HuaweiProvider)(nil)

// NewHuaweiProvider creates a new HuaweiProvider with the given credentials.
// region and endpoint are required by the OBS SDK for request signing;
// roleARN is the agency name (NOT an ARN — see ProviderConfig.RoleARN doc
// comment). When roleARN is non-empty, the provider can issue STS credentials
// via IAM Agency; when empty, GetSTSToken returns an explicit error.
//
// domainID is the Huawei account UID (numeric) required by both the IAM
// global-credentials builder AND by CreateTemporaryAccessKeyByAgency's
// IdentityAssumerole.DomainId field to issue agency-scoped temp tokens.
// Callers extract it from config at construction time. When roleARN is
// empty, domainID is still required for the global credentials builder.
func NewHuaweiProvider(endpoint, accessKey, secretKey, roleARN, domainID, region string) (*HuaweiProvider, error) {
	obsClient, err := obs.New(accessKey, secretKey, normalizeEndpoint(endpoint),
		obs.WithRegion(region),
		obs.WithSignature(obs.SignatureObs),
	)
	if err != nil {
		return nil, fmt.Errorf("create obs client: %w", err)
	}
	p := &HuaweiProvider{
		client:    obsClient,
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		domainID:  domainID,
		roleARN:   roleARN,
	}
	if roleARN != "" {
		stsCli, err := newSTSClient(&stsClientOpts{
			AccessKey: accessKey,
			SecretKey: secretKey,
			DomainID:  domainID,
			Region:    region,
			Endpoint:  iamEndpointFor(region),
		})
		if err != nil {
			return nil, fmt.Errorf("create iam client: %w", err)
		}
		p.stsCli = stsCli
	}
	return p, nil
}

// PutObject uploads data to the specified bucket and key.
func (p *HuaweiProvider) PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...types.PutOption) error {
	_ = ctx // OBS SDK v3.26.3 PutObject has no context parameter
	putOpts := types.NewPutOptions(opts...)
	// PutObjectInput embeds Bucket/Key via two layers of anonymous structs
	// (PutObjectBasicInput.ObjectOperationInput), so they can't be set in a
	// struct literal. Build the input then assign fields by name.
	input := &obs.PutObjectInput{}
	input.Bucket = bucket
	input.Key = key
	input.Body = reader
	if putOpts.ContentType != "" {
		input.ContentType = putOpts.ContentType
	}
	if _, err := p.client.PutObject(input); err != nil {
		return fmt.Errorf("put object %q: %w", key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket and key.
// The caller must close the returned reader.
func (p *HuaweiProvider) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	_ = ctx // OBS SDK v3.26.3 GetObject has no context parameter
	output, err := p.client.GetObject(&obs.GetObjectInput{
		GetObjectMetadataInput: obs.GetObjectMetadataInput{
			Bucket: bucket,
			Key:    key,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get object %q: %w", key, err)
	}
	return output.Body, nil
}

// DeleteObject removes an object from the specified bucket and key.
func (p *HuaweiProvider) DeleteObject(ctx context.Context, bucket, key string) error {
	_ = ctx // OBS SDK v3.26.3 DeleteObject has no context parameter
	if _, err := p.client.DeleteObject(&obs.DeleteObjectInput{
		Bucket: bucket,
		Key:    key,
	}); err != nil {
		return fmt.Errorf("delete object %q: %w", key, err)
	}
	return nil
}

// HeadObject retrieves metadata for an object without downloading its body.
// When the object is absent, the wrapped error satisfies
// errors.Is(err, types.ErrObjectNotFound).
//
// The OBS SDK's GetObjectMetadata does not return the x-obs-acl header, so a
// follow-up GetObjectAcl call is made to populate ObjectACL. The upload
// service relies on this field to detect ACL violations on private sessions,
// so the extra round trip is intentional.
func (p *HuaweiProvider) HeadObject(ctx context.Context, bucket, key string) (*types.ObjectInfo, error) {
	_ = ctx // OBS SDK v3.26.3 GetObjectMetadata has no context parameter
	head, err := p.client.GetObjectMetadata(&obs.GetObjectMetadataInput{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		if isHuaweiNotFound(err) {
			return nil, fmt.Errorf("head object %q: %w", key, types.ErrObjectNotFound)
		}
		return nil, fmt.Errorf("head object %q: %w", key, err)
	}

	info := objectInfoFromHead(key, head)

	// GetObjectAcl is best-effort: if it fails (e.g. permission denied on the
	// ACL subresource), we still return the rest of the metadata with an empty
	// ObjectACL rather than failing the entire HeadObject call.
	aclResp, aclErr := p.client.GetObjectAcl(&obs.GetObjectAclInput{
		Bucket: bucket,
		Key:    key,
	})
	if aclErr == nil && aclResp != nil && aclResp.Grants != nil {
		// OBS exposes ACL as a list of {Grantee, Permission} pairs. We treat
		// a single "AllUsers + READ" grant as "public-read" and everything
		// else as "private" — matches the upload service's binary check.
		info.ObjectACL = huaweiACLOrPrivate(aclResp.Grants)
	}

	return info, nil
}

// PresignPutObject generates a presigned URL for uploading an object.
// Options signed into the URL require the client to send matching headers.
//
// The OBS SDK exposes presigning via CreateSignedUrl (not the
// CreateBrowserPresignedUrl referenced in some docs); we surface the
// SDK-computed ActualSignedRequestHeaders so callers can forward them to
// the client. Without these headers the client's upload fails signature
// validation.
func (p *HuaweiProvider) PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.PutPresignOption) (string, http.Header, error) {
	_ = ctx // OBS SDK v3.26.3 CreateSignedUrl has no context parameter
	putOpts := types.NewPutPresignOptions(opts...)
	input := &obs.CreateSignedUrlInput{
		Bucket:  bucket,
		Key:     key,
		Method:  obs.HttpMethodPut,
		Expires: int(ttl.Seconds()),
	}
	if putOpts.ContentType != "" {
		input.Headers = map[string]string{
			"Content-Type": putOpts.ContentType,
		}
	}
	if putOpts.CacheControl != "" {
		if input.Headers == nil {
			input.Headers = map[string]string{}
		}
		input.Headers["Cache-Control"] = putOpts.CacheControl
	}
	for k, v := range putOpts.Metadata {
		if input.Headers == nil {
			input.Headers = map[string]string{}
		}
		// OBS user-metadata header prefix is x-obs-meta- (vs x-oss-meta- on
		// Aliyun, x-amz-meta- on S3). Signed headers must match what the
		// client sends on upload or OBS rejects with signature mismatch.
		input.Headers["x-obs-meta-"+k] = v
	}

	output, err := p.client.CreateSignedUrl(input)
	if err != nil {
		return "", nil, fmt.Errorf("sign put url for %q: %w", key, err)
	}

	// Surface the SDK-computed signed headers so callers can forward them to
	// the client. We prefer ActualSignedRequestHeaders over our input map
	// because the SDK may add headers (e.g. Host) during signing.
	var headers http.Header
	if len(output.ActualSignedRequestHeaders) > 0 {
		headers = make(http.Header, len(output.ActualSignedRequestHeaders))
		for k, vs := range output.ActualSignedRequestHeaders {
			for _, v := range vs {
				headers.Add(k, v)
			}
		}
	}
	return output.SignedUrl, headers, nil
}

// PresignGetObject generates a presigned URL for downloading an object.
//
// When WithPublic() is passed, returns an unsigned URL of the form
// https://<bucket>.<endpoint>/<key>. The caller MUST verify the object's
// bucket ACL is "public_read" before requesting this mode.
func (p *HuaweiProvider) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.GetPresignOption) (string, error) {
	_ = ctx // OBS SDK v3.26.3 CreateSignedUrl has no context parameter
	getOpts := types.NewGetPresignOptions(opts...)
	if getOpts.Public {
		return publicObjectURL(p.endpoint, bucket, key), nil
	}
	input := &obs.CreateSignedUrlInput{
		Bucket:  bucket,
		Key:     key,
		Method:  obs.HttpMethodGet,
		Expires: int(ttl.Seconds()),
	}
	queryParams := map[string]string{}
	if getOpts.Filename != "" {
		queryParams["response-content-disposition"] = types.BuildContentDisposition(getOpts.Filename)
	}
	if getOpts.ResponseContentType != "" {
		queryParams["response-content-type"] = getOpts.ResponseContentType
	}
	if getOpts.ResponseCacheControl != "" {
		queryParams["response-cache-control"] = getOpts.ResponseCacheControl
	}
	if len(getOpts.ImageOps) > 0 {
		// OBS exposes image processing via the x-image-process query
		// parameter (vs Aliyun's x-oss-process). buildObsProcessStyle
		// produces the same image/<action>,k_v syntax both vendors share.
		queryParams["x-image-process"] = buildObsProcessStyle(getOpts.ImageOps)
	}
	if len(queryParams) > 0 {
		input.QueryParams = queryParams
	}

	output, err := p.client.CreateSignedUrl(input)
	if err != nil {
		return "", fmt.Errorf("sign get url for %q: %w", key, err)
	}
	return output.SignedUrl, nil
}

// ListObjects lists all objects under the given prefix in the specified bucket.
// Paginates internally by following the Marker/NextMarker field until the OBS
// response reports IsTruncated=false.
func (p *HuaweiProvider) ListObjects(ctx context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	_ = ctx // OBS SDK v3.26.3 ListObjects has no context parameter
	var result []types.ObjectInfo
	var marker string
	for {
		input := &obs.ListObjectsInput{}
		input.Bucket = bucket
		input.Prefix = prefix
		if marker != "" {
			input.Marker = marker
		}
		page, err := p.client.ListObjects(input)
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
		// OBS only populates NextMarker when IsTruncated=true; fall back to
		// the last key of the page if the SDK leaves it empty (defensive —
		// the SDK has been observed to omit NextMarker on some responses).
		marker = page.NextMarker
		if marker == "" && len(page.Contents) > 0 {
			marker = page.Contents[len(page.Contents)-1].Key
		}
	}
	return result, nil
}

// GetSTSToken retrieves temporary credentials via IAM Agency
// (CreateTemporaryAccessKeyByAgency). Requires roleARN to be configured at
// NewHuaweiProvider time; otherwise returns an explicit error so callers know
// to use GenerateUploadURL instead.
//
// RoleSessionName embeds OwnerID so OBS audit logs can trace credentials back
// to the originating user. OwnerID is not sensitive.
func (p *HuaweiProvider) GetSTSToken(ctx context.Context, policy *types.STSPolicy) (*types.STSCredential, error) {
	if p == nil || p.stsCli == nil || p.roleARN == "" {
		return nil, fmt.Errorf("huawei STS not configured for this provider; set provider.role_arn to the agency name in config")
	}
	if policy == nil {
		return nil, fmt.Errorf("nil sts policy")
	}

	policyJSON, err := buildHuaweiPolicy(policy)
	if err != nil {
		return nil, fmt.Errorf("build sts policy: %w", err)
	}

	duration := int32(policy.TTL.Seconds())
	if duration <= 0 {
		return nil, fmt.Errorf("sts policy: TTL must be > 0")
	}
	// Huawei IAM enforces [900, 43200]s. Fail fast here so callers get an
	// actionable message instead of a wrapped SDK error from the cloud.
	if duration < minHuaweiSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v below Huawei minimum of %ds",
			policy.TTL, minHuaweiSTSDuration)
	}
	if duration > maxHuaweiSTSDuration {
		return nil, fmt.Errorf("sts policy: TTL %v above Huawei maximum of %ds",
			policy.TTL, maxHuaweiSTSDuration)
	}

	resp, err := p.stsCli.assumeAgency(ctx, &assumeAgencyReq{
		AgencyName:      p.roleARN,  // roleARN carries the agency NAME on Huawei
		DomainID:        p.domainID, // forwarded to IdentityAssumerole.DomainId
		RoleSessionName: fmt.Sprintf("owner-%d", policy.OwnerID),
		DurationSeconds: duration,
		Policy:          policyJSON,
	})
	if err != nil {
		return nil, fmt.Errorf("huawei iam create temp access key by agency: %w", err)
	}

	return &types.STSCredential{
		AccessKey:       resp.AccessKey,
		SecretKey:       resp.SecretKey,
		SecurityToken:   resp.SecurityToken,
		Endpoint:        p.endpoint,
		Region:          p.region,
		Bucket:          policy.Bucket,
		ObjectKeyPrefix: policy.KeyPrefix,
		ExpiresAt:       resp.ExpiresAt,
	}, nil
}

// --- internal helpers ---

// objectInfoFromHead translates the OBS GetObjectMetadataOutput into a
// types.ObjectInfo. ObjectACL is left empty here; HeadObject fills it via a
// separate GetObjectAcl call.
func objectInfoFromHead(key string, head *obs.GetObjectMetadataOutput) *types.ObjectInfo {
	return &types.ObjectInfo{
		Key:          key,
		Size:         head.ContentLength,
		ETag:         strings.Trim(head.ETag, `"`),
		ContentType:  head.ContentType,
		LastModified: head.LastModified,
	}
}

// isHuaweiNotFound reports whether err is a Huawei OBS "object/bucket absent"
// response. The OBS SDK surfaces 404s as obs.ObsError with StatusCode==404
// (BaseModel.StatusCode).
func isHuaweiNotFound(err error) bool {
	var obsErr obs.ObsError
	if errors.As(err, &obsErr) {
		return obsErr.StatusCode == http.StatusNotFound
	}
	return false
}

// huaweiACLOrPrivate maps an OBS grant list to the canonical ACL name. OBS
// does not return a simple "private|public-read" string the way Aliyun does —
// it returns a list of {Grantee, Permission} pairs. We treat any
// "AllUsers + READ" grant as public-read; everything else is private.
func huaweiACLOrPrivate(grants []obs.Grant) string {
	for _, g := range grants {
		if g.Grantee.URI == obs.GroupAllUsers && g.Permission == obs.PermissionRead {
			return types.ObjectACLPublicRead
		}
	}
	return types.ObjectACLPrivate
}

// publicObjectURL builds the unsigned URL for a public-read OBS object:
// https://<bucket>.<endpoint>/<key>. The endpoint is normalized so callers
// may pass it with or without a scheme, and with or without a trailing slash.
func publicObjectURL(endpoint, bucket, key string) string {
	ep := endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	ep = strings.TrimSuffix(ep, "/")
	if strings.HasPrefix(ep, "https://") || strings.HasPrefix(ep, "http://") {
		scheme := ep[:strings.Index(ep, "://")+3]
		host := ep[strings.Index(ep, "://")+3:]
		// OBS uses <bucket>.<endpoint> virtual-host style for public URLs.
		return scheme + bucket + "." + host + "/" + strings.TrimPrefix(key, "/")
	}
	return ep + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
}

// normalizeEndpoint ensures the endpoint has a scheme — the OBS SDK requires
// "https://obs.<region>.myhuaweicloud.com" form. Empty input is left empty
// so the SDK falls back to its region-derived endpoint.
func normalizeEndpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if !strings.Contains(endpoint, "://") {
		return "https://" + endpoint
	}
	return endpoint
}
