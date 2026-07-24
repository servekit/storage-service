package xcodes

import "github.com/servekit/go-common/xerr"

// CDN error codes for the GenerateCDNURL RPC.
var (
	// ErrCDNNotConfigured: the bucket's provider has no CDN configured
	// (ProviderConfig.CDN is nil for that provider). Client should fall back
	// to GenerateDownloadURL / GenerateProcessURL.
	ErrCDNNotConfigured = xerr.New("CDN_NOT_CONFIGURED", xerr.CategoryBadRequest, 400, "CDN not configured for this provider")

	// ErrCDNImageProcessingUnsupported: caller passed non-empty ops to a
	// provider that can't process images at the CDN/origin layer (currently
	// S3+CloudFront). Client should retry without ops or use a different
	// provider for image processing.
	ErrCDNImageProcessingUnsupported = xerr.New("CDN_IMAGE_PROCESSING_UNSUPPORTED", xerr.CategoryBadRequest, 400, "image processing not supported by this CDN provider")
)
