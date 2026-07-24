package types

import (
	"errors"
	"net/url"
	"strings"
)

// ErrImageProcessingUnsupported is returned by providers that lack cloud-side
// image processing when a caller passes WithImageOps with non-empty ops. S3
// always returns this; Aliyun and Tencent COS support image processing.
var ErrImageProcessingUnsupported = errors.New("image processing not supported by this provider")

// GetPresignOptions configures PresignGetObject. Empty fields mean "no
// override" — the cloud returns the object as-stored.
type GetPresignOptions struct {
	// Filename sets response-content-disposition so browsers save the download
	// with this name instead of deriving one from the URL path (which is
	// usually the MD5-derived object key on this project).
	Filename string

	// ResponseContentType overrides the Content-Type the cloud returns. Useful
	// for forcing display vs. download behavior.
	ResponseContentType string

	// ResponseCacheControl overrides Cache-Control on the response.
	ResponseCacheControl string

	// ImageOps applies cloud-side image processing. Each vendor translates the
	// ops into its own syntax (Aliyun x-oss-process, Tencent CI imageMogr2)
	// inside PresignGetObject. Empty slice means no processing. S3 rejects
	// non-empty ImageOps with ErrImageProcessingUnsupported.
	ImageOps []Op

	// Public, when true, makes PresignGetObject return an unsigned URL
	// (https://<bucket>.<endpoint>/<key>) instead of a presigned one.
	// Only valid for public-read buckets; the caller is responsible for
	// verifying that (typically via StorageObject.IsPublic derived from
	// BucketConfig.ACL == "public_read").
	Public bool
}

// GetPresignOption configures a GetPresignOptions.
type GetPresignOption func(*GetPresignOptions)

// WithDownloadFilename sets the filename browsers save the download as.
func WithDownloadFilename(name string) GetPresignOption {
	return func(o *GetPresignOptions) { o.Filename = name }
}

// WithResponseContentType overrides the Content-Type returned by the cloud.
func WithResponseContentType(ct string) GetPresignOption {
	return func(o *GetPresignOptions) { o.ResponseContentType = ct }
}

// WithResponseCacheControl overrides the Cache-Control returned by the cloud.
func WithResponseCacheControl(cc string) GetPresignOption {
	return func(o *GetPresignOptions) { o.ResponseCacheControl = cc }
}

// WithImageOps applies cloud-side image processing. The vendor translates the
// typed ops into its native syntax inside PresignGetObject. S3 returns
// ErrImageProcessingUnsupported because it has no equivalent.
func WithImageOps(ops []Op) GetPresignOption {
	return func(o *GetPresignOptions) { o.ImageOps = ops }
}

// WithPublic makes PresignGetObject return an unsigned URL for public-read
// buckets instead of a presigned one. Only valid when the underlying object's
// bucket ACL is public_read; the caller must verify that before requesting.
func WithPublic() GetPresignOption {
	return func(o *GetPresignOptions) { o.Public = true }
}

// NewGetPresignOptions applies the given options and returns a GetPresignOptions.
func NewGetPresignOptions(opts ...GetPresignOption) *GetPresignOptions {
	o := &GetPresignOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// PutPresignOptions configures PresignPutObject. Set fields become part of the
// signed URL — clients must send matching headers or the upload is rejected.
type PutPresignOptions struct {
	// ContentType is signed into the URL. Client must send identical
	// Content-Type header or OSS rejects with signature mismatch.
	ContentType string

	// CacheControl is signed into the URL as Cache-Control.
	CacheControl string

	// Metadata is user-defined metadata, surfaced as x-oss-meta-<key> on
	// Aliyun and x-amz-meta-<key> on S3.
	Metadata map[string]string
}

// PutPresignOption configures a PutPresignOptions.
type PutPresignOption func(*PutPresignOptions)

// WithContentType signs the URL with the given Content-Type.
//
// Named WithUploadContentType (not WithContentType) to avoid colliding with
// the existing types.WithContentType PutOption used for server-side PutObject.
func WithUploadContentType(ct string) PutPresignOption {
	return func(o *PutPresignOptions) { o.ContentType = ct }
}

// WithCacheControl signs the URL with the given Cache-Control.
func WithUploadCacheControl(cc string) PutPresignOption {
	return func(o *PutPresignOptions) { o.CacheControl = cc }
}

// WithMetadata signs user-defined metadata into the URL.
func WithUploadMetadata(kv map[string]string) PutPresignOption {
	return func(o *PutPresignOptions) { o.Metadata = kv }
}

// NewPutPresignOptions applies the given options and returns a PutPresignOptions.
func NewPutPresignOptions(opts ...PutPresignOption) *PutPresignOptions {
	o := &PutPresignOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// --- internal helpers ---

// BuildContentDisposition constructs a Content-Disposition value that forces
// browsers to save the download with the given filename. Implements RFC 6266 /
// RFC 5987 to handle non-ASCII filenames safely:
//
//   - ASCII-only filenames emit `attachment; filename="<name>"`
//   - Non-ASCII filenames additionally emit `filename*=UTF-8”<pct-encoded>`
//
// Shared across providers so behavior is identical regardless of backend.
// Empty filename returns empty (caller should not set the header at all).
func BuildContentDisposition(filename string) string {
	if filename == "" {
		return ""
	}
	if isASCII(filename) {
		// Escape any embedded quotes so the filename attribute parses cleanly.
		escaped := strings.ReplaceAll(filename, `"`, `\"`)
		return `attachment; filename="` + escaped + `"`
	}
	// filename* per RFC 5987: token '' (no language tag) + percent-encoded UTF-8.
	// Include an ASCII fallback when derivable so very old clients still work.
	return `attachment; filename*=UTF-8''` + url.PathEscape(filename)
}

// isASCII reports whether s contains only code points <= 0x7f.
func isASCII(s string) bool {
	for _, r := range s {
		if r > 0x7f {
			return false
		}
	}
	return true
}
