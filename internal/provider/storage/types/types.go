// Package types defines the vendor-agnostic storage provider contract: the
// Provider interface, STS policy/credential structs, and PutObject options.
//
// Both the parent storage package and vendor-specific subpackages (e.g. aliyun/)
// depend on these types. Keeping them in a leaf package avoids the import cycle
// storage → aliyun → storage that would otherwise arise when aliyun code needs
// to reference the Provider interface.
package types

import (
	"context"
	"io"
	"net/http"
	"time"
)

// Provider defines the interface for cloud storage operations.
type Provider interface {
	PutObject(ctx context.Context, bucket, key string, reader io.Reader, size int64, opts ...PutOption) error
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	DeleteObject(ctx context.Context, bucket, key string) error
	HeadObject(ctx context.Context, bucket, key string) (*ObjectInfo, error)
	PresignPutObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...PutPresignOption) (string, http.Header, error)
	PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration, opts ...GetPresignOption) (string, error)
	GetSTSToken(ctx context.Context, policy *STSPolicy) (*STSCredential, error)
	ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error)
}

// ObjectInfo holds metadata about a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
	// ObjectACL is the cloud-side ACL applied to the object. Empty when the
	// provider did not surface it (e.g. HeadObject didn't include the ACL
	// header) or when the object inherits the bucket default ("default" on
	// Aliyun). Use the ObjectACL* constants for comparison, never raw strings.
	ObjectACL string
}

// PutOption configures a PutObject call.
type PutOption func(*PutOptions)

// PutOptions holds optional parameters for PutObject.
type PutOptions struct {
	ContentType string
}

// Canonical object ACL values recognized across vendors. Use these constants
// instead of raw strings so a typo fails at compile time rather than silently
// letting a public-read object slip past an audit.
const (
	// ObjectACLPrivate: only the bucket owner can read. The default for new
	// objects in well-configured buckets; pair with LockObjectACL on STSPolicy
	// to enforce this server-side.
	ObjectACLPrivate = "private"
	// ObjectACLPublicRead: anyone can read anonymously. Reserve for assets
	// that must be publicly accessible (e.g. CDN front-end); never use for
	// user-uploaded content.
	ObjectACLPublicRead = "public-read"
	// ObjectACLPublicReadWrite: anyone can read AND overwrite. Almost never
	// appropriate; included for completeness.
	ObjectACLPublicReadWrite = "public-read-write"
)

// WithContentType sets the content type for the object.
func WithContentType(ct string) PutOption {
	return func(o *PutOptions) { o.ContentType = ct }
}

// NewPutOptions applies the given options and returns a PutOptions.
func NewPutOptions(opts ...PutOption) *PutOptions {
	o := &PutOptions{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Proto Vendor enum values (see gen/storage/v1/storage.pb.go). Tracked here
// as constants rather than importing the generated proto package to avoid
// dragging the proto dependency into this leaf types package.
const (
	vendorAliyunOSS     int32 = 1 // VENDOR_ALIYUN_OSS
	vendorAWSS3         int32 = 2 // VENDOR_AWS_S3
	vendorS3Compatible  int32 = 3 // VENDOR_S3_COMPATIBLE
	vendorTencentCOS    int32 = 4 // VENDOR_TENCENT_COS
	vendorHuaweiOBS     int32 = 5 // VENDOR_HUAWEI_OBS
	vendorVolcengineTOS int32 = 6 // VENDOR_VOLCENGINE_TOS
)

// PutObjectActionForVendor returns the PutObject action string the vendor's
// STS engine expects on its Allow statement. Each cloud provider namespaces
// actions under a vendor-specific prefix; using the wrong prefix causes the
// cloud STS API to reject the policy (or mint a credential that can't actually
// upload). The values mirror the defaults each vendor's STS builder uses
// (see internal/provider/storage/<vendor>/sts.go).
//
// vendor is the int32 value of the proto Vendor enum. Unknown values default
// to `s3:PutObject` — fails open for forward-compat with new S3-compatible
// vendors (which is the most common case for new backends).
func PutObjectActionForVendor(vendor int32) string {
	switch vendor {
	case vendorAliyunOSS:
		return "oss:PutObject"
	case vendorTencentCOS:
		// Tencent CAM requires the literal "name/cos:" prefix on action
		// strings; bare "cos:PutObject" is rejected.
		return "name/cos:PutObject"
	case vendorHuaweiOBS:
		// Huawei IAM uses the "obs:object:<Action>" pattern for object-level
		// operations; "obs:PutObject" alone is rejected.
		return "obs:object:PutObject"
	case vendorVolcengineTOS:
		return "tos:PutObject"
	default:
		// VENDOR_AWS_S3, VENDOR_S3_COMPATIBLE, and any future S3-compatible
		// backend all use the AWS IAM "s3:" prefix.
		return "s3:PutObject"
	}
}
