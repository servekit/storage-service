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
	// ErrSecretRetired refuses the config-secret rotation RPC: the
	// app_secret column was dropped when the ④ window closed (spec §9.1.3)
	// — config rows carry no credential; the data plane authenticates via
	// the trusted x-tenant-key the doors inject.
	ErrSecretRetired = xerr.New("SECRET_RETIRED", xerr.CategoryBadRequest, 400, "app_secret was retired; the data plane authenticates via x-tenant-key")
)
