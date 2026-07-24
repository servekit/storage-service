// Package storage implements the storage provider registry and vendor-specific
// provider implementations (S3, Fake). The vendor-agnostic contract types
// (Provider interface, STSPolicy, etc.) live in the types/ subpackage and are
// re-exported here as aliases so existing callers can keep using storage.Provider,
// storage.STSPolicy, etc.
package storage

import (
	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// Re-exported contract types. Definitions live in types/ so vendor-specific
// subpackages (e.g. aliyun/) can depend on them without an import cycle.
type (
	Provider          = types.Provider
	ObjectInfo        = types.ObjectInfo
	PutOption         = types.PutOption
	PutOptions        = types.PutOptions
	GetPresignOption  = types.GetPresignOption
	GetPresignOptions = types.GetPresignOptions
	PutPresignOption  = types.PutPresignOption
	PutPresignOptions = types.PutPresignOptions
)

// WithContentType sets the content type for the object.
// Forwarded to types.WithContentType (Go does not support function aliases).
func WithContentType(ct string) PutOption { return types.WithContentType(ct) }

// NewPutOptions applies the given options and returns a PutOptions.
// Forwarded to types.NewPutOptions (Go does not support function aliases).
func NewPutOptions(opts ...PutOption) *PutOptions { return types.NewPutOptions(opts...) }

// Presign GET option forwarders.
func WithDownloadFilename(name string) GetPresignOption   { return types.WithDownloadFilename(name) }
func WithResponseContentType(ct string) GetPresignOption  { return types.WithResponseContentType(ct) }
func WithResponseCacheControl(cc string) GetPresignOption { return types.WithResponseCacheControl(cc) }
func WithImageOps(ops []types.Op) GetPresignOption        { return types.WithImageOps(ops) }

// WithPublic forwards to types.WithPublic. Use to request an unsigned URL
// for a public-read bucket's object instead of a presigned one.
func WithPublic() GetPresignOption { return types.WithPublic() }

// Presign PUT option forwarders. Named WithUpload* to avoid colliding with
// WithContentType above.
func WithUploadContentType(ct string) PutPresignOption  { return types.WithUploadContentType(ct) }
func WithUploadCacheControl(cc string) PutPresignOption { return types.WithUploadCacheControl(cc) }
func WithUploadMetadata(kv map[string]string) PutPresignOption {
	return types.WithUploadMetadata(kv)
}
