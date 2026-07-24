package xcodes

import "github.com/servekit/go-common/xerr"

// Storage and file error codes.
var (
	ErrInternal                = xerr.New("INTERNAL", xerr.CategoryInternal, 500, "internal error")
	ErrBadRequest              = xerr.New("BAD_REQUEST", xerr.CategoryBadRequest, 400, "bad request")
	ErrFileNotFound            = xerr.New("FILE_NOT_FOUND", xerr.CategoryNotFound, 404, "file not found")
	ErrFileNotActive           = xerr.New("FILE_NOT_ACTIVE", xerr.CategoryNotFound, 404, "file not found or already deleted")
	ErrFileBatchTooLarge       = xerr.New("FILE_BATCH_TOO_LARGE", xerr.CategoryBadRequest, 400, "file batch size too large")
	ErrMD5Mismatch             = xerr.New("MD5_MISMATCH", xerr.CategoryBadRequest, 400, "md5 checksum mismatch")
	ErrFileSizeExceeded        = xerr.New("FILE_SIZE_EXCEEDED", xerr.CategoryBadRequest, 400, "file size exceeded")
	ErrUploadTokenExpired      = xerr.New("UPLOAD_TOKEN_EXPIRED", xerr.CategoryBadRequest, 400, "upload token expired")
	ErrUploadTokenInvalid      = xerr.New("UPLOAD_TOKEN_INVALID", xerr.CategoryUnauthorized, 401, "upload token invalid")
	ErrObjectNotFound          = xerr.New("OBJECT_NOT_FOUND", xerr.CategoryNotFound, 404, "storage object not found")
	ErrRateLimited             = xerr.New("RATE_LIMITED", xerr.CategoryTooManyRequests, 429, "rate limit exceeded")
	ErrUploadSessionNotFound   = xerr.New("UPLOAD_SESSION_NOT_FOUND", xerr.CategoryNotFound, 404, "upload session not found")
	ErrUploadSessionNotPending = xerr.New("UPLOAD_SESSION_NOT_PENDING", xerr.CategoryConflict, 409, "upload session is not pending")
	ErrUploadSessionExpired    = xerr.New("UPLOAD_SESSION_EXPIRED", xerr.CategoryBadRequest, 400, "upload session expired or cancelled")
	ErrContentTypeMismatch     = xerr.New("CONTENT_TYPE_MISMATCH", xerr.CategoryBadRequest, 400, "declared content type does not match the uploaded object")
	ErrObjectACLViolation      = xerr.New("OBJECT_ACL_VIOLATION", xerr.CategoryBadRequest, 400, "uploaded object has an ACL that violates the session policy")
)
