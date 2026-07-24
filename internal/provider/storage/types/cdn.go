package types

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// SchemeHTTPS pins the CDN URL scheme. The service only supports https CDN
// distribution — http is intentionally not configurable. Both the config
// validator (validateCDNDomain rejects scheme prefixes on Domain) and
// CDNObjectURL rely on this invariant.
const SchemeHTTPS = "https"

// CDNObjectURL builds the canonical https URL for an object on a CDN domain.
// objectKey MUST NOT start with "/"; the helper adds the leading slash.
//
// Centralizing this so all providers agree on scheme + path layout. To change
// scheme for everyone, edit SchemeHTTPS — do not construct url.URL inline in
// provider code.
func CDNObjectURL(domain, objectKey string) *url.URL {
	return &url.URL{
		Scheme: SchemeHTTPS,
		Host:   domain,
		Path:   "/" + objectKey,
	}
}

// ErrCDNNotConfigured is returned when a bucket has no CDN configured
// (BucketConfig.CDN is nil). Callers should fall back to
// GenerateDownloadURL / GenerateProcessURL (presigned URL paths).
var ErrCDNNotConfigured = fmt.Errorf("cdn: not configured for this bucket")

// ErrCDNImageProcessingUnsupported is returned when the bucket's CDN does
// not support image processing at the CDN/origin layer (currently
// S3+CloudFront). Callers should retry without ops, or use
// GenerateProcessURL against a bucket that does support it (Aliyun OSS+CDN).
var ErrCDNImageProcessingUnsupported = fmt.Errorf("cdn: image processing not supported by this bucket's CDN")

// CDNURLOptions configures a single CDNURL call. Fields default to zero
// values; nil/empty means "don't apply".
type CDNURLOptions struct {
	// Ops is the optional list of image processing operations (Aliyun
	// x-oss-process). S3+CloudFront rejects non-empty Ops with
	// ErrCDNImageProcessingUnsupported.
	Ops []Op

	// TTL is the signature lifetime. Ignored when Public=true. Service layer
	// clamps it to [cdn.min_ttl, cdn.max_ttl] before calling.
	TTL time.Duration

	// Public, when true, asks for an unsigned permanent URL. The caller must
	// ensure the underlying file is_public and CDN console allows anonymous
	// access for the path. TTL is ignored in this mode.
	Public bool

	// Filename, when non-empty, adds response-content-disposition=attachment
	// to the URL query so browsers download with this filename instead of
	// deriving one from the URL path (which is usually the MD5-derived object
	// key on this project).
	//
	// CDN deployment requirements for this to actually take effect at the
	// browser:
	//   - Aliyun CDN: console must forward response-content-disposition to
	//     OSS origin (default behavior, but check "Filter Parameters").
	//   - CloudFront: distribution must attach an Origin Request Policy that
	//     forwards response-content-disposition (e.g. the managed
	//     AllViewerExceptsHostHeader, or a custom one). Without it, the query
	//     is silently dropped at the edge.
	//   - Both: response-content-disposition should be EXCLUDED from the CDN
	//     cache key (otherwise different filenames fragment the cache).
	Filename string
}

// CDNURLGenerator builds CDN-fronted signed URLs for objects on a specific
// bucket. Each instance is constructed with one bucket's CDNConfig — the
// generator IS bound to the bucket, so CDNURL takes no bucket parameter.
//
// Implementations:
//   - Aliyun Type A MD5 auth_key (vendor=VENDOR_ALIYUN_OSS)
//   - AWS CloudFront Signed URL (vendor=VENDOR_AWS_S3 or VENDOR_S3_COMPATIBLE, RSA)
//
// The Registry instantiates the right one per bucket based on the provider's
// vendor and BucketConfig.CDN; service code never picks a vendor directly.
type CDNURLGenerator interface {
	// CDNURL returns a CDN URL for the object this generator is bound to.
	//
	// When opts.Public=false (default): the URL carries a signature and
	// expires at (now + opts.TTL).
	//
	// When opts.Public=true: the URL is unsigned (no auth_key/Signature query
	// params) and permanent (expiresAt returns the zero time.Time). The
	// caller is responsible for ensuring the underlying file is public
	// (file.is_public=true) and that the CDN is configured to allow anonymous
	// access for the path. opts.TTL is ignored in this mode.
	//
	// opts.Filename, when non-empty, adds response-content-disposition to the
	// URL query — see CDNURLOptions.Filename for CDN-side requirements.
	//
	// Returns:
	//   - ErrCDNImageProcessingUnsupported if opts.Ops is non-empty and the
	//     CDN/origin can't process images.
	//   - Other errors for internal failures (signing key missing, etc.).
	CDNURL(ctx context.Context, objectKey string, opts CDNURLOptions) (url string, expiresAt time.Time, err error)
}
