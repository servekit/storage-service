package xcodes

import "github.com/servekit/go-common/xerr"

var (
	ErrProviderNotFound     = xerr.New("PROVIDER_NOT_FOUND", xerr.CategoryNotFound, 404, "storage provider not found")
	ErrBucketNotFound       = xerr.New("BUCKET_NOT_FOUND", xerr.CategoryNotFound, 404, "bucket not found")
	ErrBucketVendorMismatch = xerr.New("BUCKET_VENDOR_MISMATCH", xerr.CategoryBadRequest, 400, "bucket does not belong to the specified vendor")
)
