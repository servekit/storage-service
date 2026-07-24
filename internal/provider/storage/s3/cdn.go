package s3

import (
	"context"
	"fmt"
	"time"

	"github.com/servekit/storage-service/internal/provider/storage/types"
	"github.com/servekit/storage-service/pkg/config"

	"github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign"
)

// CDNURLGenerator builds CloudFront-signed URLs for objects on a single
// bucket. One instance per bucket; constructed by the Registry from the
// bucket's config.CDNConfig. Decoupled from S3Provider so the storage
// provider stays focused on S3 operations — CDN signing has nothing to do
// with S3 itself (CloudFront is a separate service that just happens to use
// S3 as its origin).
type CDNURLGenerator struct {
	cdnConfig *config.CDNConfig
}

// Compile-time assertion that *CDNURLGenerator satisfies types.CDNURLGenerator.
var _ types.CDNURLGenerator = (*CDNURLGenerator)(nil)

// NewCDNURLGenerator constructs a CloudFront CDNURLGenerator for a bucket.
// cdn MUST be non-nil — callers (Registry) gate on nil before constructing.
func NewCDNURLGenerator(cdn *config.CDNConfig) *CDNURLGenerator {
	return &CDNURLGenerator{cdnConfig: cdn}
}

// CDNURL builds a CloudFront URL for the object this generator is bound to.
// S3+CloudFront does not support image processing at the edge; non-empty
// opts.Ops returns ErrCDNImageProcessingUnsupported (edge image processing
// would require Lambda@Edge, which is out of scope).
//
// When opts.Public=false (default): the URL is a CloudFront Signed URL using
// the Canned Policy (sign.URLSigner.Sign): the URL carries Expires,
// Signature, and Key-Pair-Id query params. The signing key (PEM private
// key file path in cdnConfig.AuthKey) and KeyPairID must match the
// trusted key pair configured on the CloudFront distribution.
//
// When opts.Public=true: the URL is unsigned (no Signature/Expires/Key-Pair-Id)
// and permanent. CloudFront distribution must be configured to allow
// anonymous access for the path.
//
// opts.Filename, when non-empty, adds response-content-disposition to the
// URL query. CloudFront does NOT forward query strings to S3 by default —
// the distribution must attach an Origin Request Policy that includes
// response-content-disposition (e.g. the managed AllViewerExceptsHostHeader
// or a custom policy). Without it, the query is silently dropped at the
// edge and S3 returns Content-Disposition from object metadata instead.
// See types.CDNURLOptions.Filename for the full deployment checklist.
//
// Query params are signed into the URL — Sign() signs the URL exactly as
// passed, so all non-Signature/Expires/Key-Pair-Id params must be set
// before calling Sign.
func (g *CDNURLGenerator) CDNURL(_ context.Context, objectKey string, opts types.CDNURLOptions) (string, time.Time, error) {
	if len(opts.Ops) > 0 {
		return "", time.Time{}, types.ErrCDNImageProcessingUnsupported
	}

	u := types.CDNObjectURL(g.cdnConfig.Domain, objectKey)
	if opts.Filename != "" {
		q := u.Query()
		q.Set("response-content-disposition", types.BuildContentDisposition(opts.Filename))
		u.RawQuery = q.Encode()
	}
	rawURL := u.String()

	if opts.Public {
		// Public URL: no Signature/Expires/Key-Pair-Id, permanent.
		// CloudFront distribution must be configured to allow anonymous
		// access for this path.
		return rawURL, time.Time{}, nil
	}

	now := time.Now()
	expiresAt := now.Add(opts.TTL)

	privKey, err := sign.LoadPEMPrivKeyFile(g.cdnConfig.AuthKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load cloudfront private key from %q: %w", g.cdnConfig.AuthKey, err)
	}
	signer := sign.NewURLSigner(g.cdnConfig.KeyPairID, privKey)
	signed, err := signer.Sign(rawURL, expiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign cloudfront url: %w", err)
	}
	return signed, expiresAt, nil
}
