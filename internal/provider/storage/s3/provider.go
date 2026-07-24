// Package s3 implements the types.Provider interface for S3-compatible
// storage backends (AWS S3, MinIO, Huawei OBS, etc.). All S3-specific code
// lives in this package so the parent storage package stays vendor-agnostic.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// S3Provider implements storage.Provider for AWS S3 and S3-compatible backends
// (MinIO, Ceph RGW, LocalStack). STS is optional — requires RoleARN at
// construction time, otherwise GetSTSToken returns "not configured".
//
// CDN URL generation lives in the standalone CDNURLGenerator type — this
// provider only handles S3 operations.
type S3Provider struct {
	client    *awss3.Client
	presigner *awss3.PresignClient
	stsCli    assumeRoleCaller // nil when roleARN == ""; GetSTSToken returns "not configured"
	roleARN   string
	region    string // surfaced via STSCredential.Region so clients don't have to derive from Endpoint
	endpoint  string
}

// NewS3Provider creates a new S3Provider with static credentials. roleArn
// enables STS via GetSTSToken when non-empty — AWS format
// `arn:aws:iam::<account-id>:role/<name>`; MinIO accepts any non-empty
// identifier. Empty roleArn = STS unavailable; callers must use
// GenerateUploadURL instead.
//
// endpoint may be passed with or without a scheme — bare `host:port` is
// normalized to `http://`. The AWS SDK rejects BaseEndpoint values that
// lack a scheme (e.g. the `host:port` shape testcontainers' MinIO
// ConnectionString returns), so we always prefix one before forwarding.
func NewS3Provider(endpoint, region, accessKey, secretKey, roleARN string) (*S3Provider, error) {
	creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")

	endpoint = normalizeS3Endpoint(endpoint)

	var opts []func(*awss3.Options)

	if endpoint != "" {
		opts = append(opts, func(o *awss3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}

	client := awss3.New(awss3.Options{
		Region:      region,
		Credentials: creds,
	}, opts...)

	p := &S3Provider{
		client:    client,
		presigner: awss3.NewPresignClient(client),
		roleARN:   roleARN,
		region:    region,
		endpoint:  endpoint,
	}

	if roleARN != "" {
		stsCli, err := newSTSClient(&stsClientOpts{
			AccessKey: accessKey,
			SecretKey: secretKey,
			Region:    region,
			Endpoint:  endpoint,
		})
		if err != nil {
			return nil, fmt.Errorf("init sts client: %w", err)
		}
		p.stsCli = stsCli
	}

	return p, nil
}

// PutObject uploads an object to the specified bucket.
func (p *S3Provider) PutObject(ctx context.Context, bucket, key string, reader io.Reader, _ int64, opts ...types.PutOption) error {
	putOpts := types.NewPutOptions(opts...)

	input := &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   reader,
	}
	if putOpts.ContentType != "" {
		input.ContentType = aws.String(putOpts.ContentType)
	}

	_, err := p.client.PutObject(ctx, input)
	if err != nil {
		return fmt.Errorf("s3 put object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// GetObject retrieves an object from the specified bucket and returns its body.
// The caller is responsible for closing the returned ReadCloser.
func (p *S3Provider) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	out, err := p.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get object %s/%s: %w", bucket, key, err)
	}
	return out.Body, nil
}

// DeleteObject removes an object from the specified bucket.
func (p *S3Provider) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := p.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// HeadObject retrieves metadata for an object without returning the object body.
// When the object is absent, the wrapped error satisfies errors.Is(err, types.ErrObjectNotFound).
func (p *S3Provider) HeadObject(ctx context.Context, bucket, key string) (*types.ObjectInfo, error) {
	out, err := p.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, fmt.Errorf("s3 head object %s/%s: %w", bucket, key, types.ErrObjectNotFound)
		}
		return nil, fmt.Errorf("s3 head object %s/%s: %w", bucket, key, err)
	}

	info := &types.ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         aws.ToString(out.ETag),
		ContentType:  aws.ToString(out.ContentType),
		LastModified: aws.ToTime(out.LastModified),
	}
	// S3 HeadObject does not return the object ACL — that requires a separate
	// GetObjectAcl API call we deliberately avoid here (latency + permission
	// surface). ObjectACL stays empty; consumers must not treat empty as
	// "private".
	return info, nil
}

// PresignPutObject generates a presigned URL for uploading an object.
// It returns the presigned URL and the signed HTTP headers.
//
// S3 supports ContentType / CacheControl / Metadata signing.
func (p *S3Provider) PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.PutPresignOption) (string, http.Header, error) {
	putOpts := types.NewPutPresignOptions(opts...)
	input := &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if putOpts.ContentType != "" {
		input.ContentType = aws.String(putOpts.ContentType)
	}
	if putOpts.CacheControl != "" {
		input.CacheControl = aws.String(putOpts.CacheControl)
	}
	if len(putOpts.Metadata) > 0 {
		input.Metadata = putOpts.Metadata
	}
	req, err := p.presigner.PresignPutObject(ctx, input, func(opts *awss3.PresignOptions) {
		opts.Expires = ttl
	})
	if err != nil {
		return "", nil, fmt.Errorf("s3 presign put object %s/%s: %w", bucket, key, err)
	}
	return req.URL, req.SignedHeader, nil
}

// PresignGetObject generates a presigned URL for downloading an object.
//
// When WithPublic() is passed, returns an unsigned URL of the form
// https://<bucket>.<endpoint>/<key> (path-style for custom endpoints, or
// virtual-host style for default AWS endpoints). The caller MUST verify the
// object's bucket ACL is "public_read" before requesting this mode.
//
// S3 supports ResponseContentDisposition (filename) / ResponseContentType /
// ResponseCacheControl overrides. WithImageOps is currently rejected with
// types.ErrImageProcessingUnsupported — S3 has no native x-oss-process.
// buildS3ProcessStyle exists as a stub so future integrations (Lambda
// transforms, CloudFront) can fill in without touching this call site.
func (p *S3Provider) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...types.GetPresignOption) (string, error) {
	getOpts := types.NewGetPresignOptions(opts...)
	if getOpts.Public {
		return publicObjectURL(p.endpoint, bucket, key), nil
	}
	if len(getOpts.ImageOps) > 0 {
		// buildS3ProcessStyle is currently a stub returning "". When non-empty,
		// wire it into the request here (e.g., as a Lambda Function URL param).
		if style := buildS3ProcessStyle(getOpts.ImageOps); style == "" {
			return "", fmt.Errorf("s3 presign get object %s/%s: %w", bucket, key, types.ErrImageProcessingUnsupported)
		}
	}
	input := &awss3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}
	if getOpts.Filename != "" {
		input.ResponseContentDisposition = aws.String(types.BuildContentDisposition(getOpts.Filename))
	}
	if getOpts.ResponseContentType != "" {
		input.ResponseContentType = aws.String(getOpts.ResponseContentType)
	}
	if getOpts.ResponseCacheControl != "" {
		input.ResponseCacheControl = aws.String(getOpts.ResponseCacheControl)
	}
	req, err := p.presigner.PresignGetObject(ctx, input, func(opts *awss3.PresignOptions) {
		opts.Expires = ttl
	})
	if err != nil {
		return "", fmt.Errorf("s3 presign get object %s/%s: %w", bucket, key, err)
	}
	return req.URL, nil
}

// ListObjects returns all objects under the given prefix in the specified bucket.
func (p *S3Provider) ListObjects(ctx context.Context, bucket, prefix string) ([]types.ObjectInfo, error) {
	var objects []types.ObjectInfo
	paginator := awss3.NewListObjectsV2Paginator(p.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		out, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list objects %s/%s: %w", bucket, prefix, err)
		}

		for _, obj := range out.Contents {
			objects = append(objects, types.ObjectInfo{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				ETag:         aws.ToString(obj.ETag),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
	}

	return objects, nil
}

// --- internal helpers ---

// normalizeS3Endpoint ensures endpoint has an http(s):// scheme. The AWS SDK
// rejects BaseEndpoint values that lack a scheme; operators (and the MinIO
// testcontainer) often pass bare `host:port`. Empty input is returned as-is
// so the SDK uses its default endpoint. Existing http(s):// prefixes are
// preserved.
func normalizeS3Endpoint(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	if strings.Contains(endpoint, "://") {
		return endpoint
	}
	// Bare host:port — default to http so MinIO/local dev works without
	// extra config. Production deployments behind TLS should pass https://
	// explicitly (or terminate TLS upstream).
	return "http://" + endpoint
}

// isS3NotFound reports whether err is an S3 "object absent" response. The AWS
// SDK v2 surfaces 404s from HeadObject as *types.NotFound and from GetObject
// as *types.NoSuchKey; both are detected so callers reliably receive the
// types.ErrObjectNotFound sentinel regardless of which API produced the error.
func isS3NotFound(err error) bool {
	var noSuchKey *awss3types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *awss3types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	return false
}

// publicObjectURL builds the unsigned URL for a public-read S3 object.
// When endpoint is empty (default AWS endpoint), uses virtual-host style:
//
//	https://<bucket>.<region or default>.s3.amazonaws.com/<key>
//
// When endpoint is set (MinIO, LocalStack, custom), uses path-style:
//
//	https://<endpoint>/<bucket>/<key>
//
// The caller MUST verify the bucket ACL is public before calling.
func publicObjectURL(endpoint, bucket, key string) string {
	key = strings.TrimPrefix(key, "/")
	if endpoint == "" {
		// Default AWS S3 — virtual-host style. Region is unknown here, but
		// the AWS default DNS resolves https://<bucket>.s3.amazonaws.com/<key>.
		return "https://" + bucket + ".s3.amazonaws.com/" + key
	}
	ep := endpoint
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	ep = strings.TrimSuffix(ep, "/")
	// Custom endpoints (MinIO etc.) typically require path-style addressing
	// — virtual-host style may not be supported.
	return ep + "/" + bucket + "/" + key
}
