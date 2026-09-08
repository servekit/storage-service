package xcodes

import "github.com/servekit/go-common/xerr"

var (
	ErrProviderNotFound     = xerr.New("PROVIDER_NOT_FOUND", xerr.CategoryNotFound, 404, "storage provider not found")
	ErrBucketNotFound       = xerr.New("BUCKET_NOT_FOUND", xerr.CategoryNotFound, 404, "bucket not found")
	ErrBucketVendorMismatch = xerr.New("BUCKET_VENDOR_MISMATCH", xerr.CategoryBadRequest, 400, "bucket does not belong to the specified vendor")
	ErrAppNotFound          = xerr.New("APP_NOT_FOUND", xerr.CategoryNotFound, 404, "storage app not found")
	ErrAppExists            = xerr.New("APP_EXISTS", xerr.CategoryConflict, 409, "storage app already exists")
	ErrAppUnauthorized      = xerr.New("APP_UNAUTHORIZED", xerr.CategoryUnauthorized, 401, "storage app missing, disabled, or bad credentials")
	ErrPrefixTaken          = xerr.New("PREFIX_TAKEN", xerr.CategoryConflict, 409, "key_prefix already in use")
)
