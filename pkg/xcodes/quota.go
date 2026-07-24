package xcodes

import "github.com/servekit/go-common/xerr"

// Quota error codes.
var (
	ErrQuotaExceeded          = xerr.New("QUOTA_EXCEEDED", xerr.CategoryForbidden, 403, "storage quota exceeded")
	ErrQuotaNotFound          = xerr.New("QUOTA_NOT_FOUND", xerr.CategoryNotFound, 404, "quota not found")
	ErrQuotaNotActive         = xerr.New("QUOTA_NOT_ACTIVE", xerr.CategoryNotFound, 404, "quota not found or deleted")
	ErrQuotaInsufficientUsed  = xerr.New("QUOTA_INSUFFICIENT_USED", xerr.CategoryConflict, 409, "insufficient used bytes")
	ErrQuotaInsufficientTotal = xerr.New("QUOTA_INSUFFICIENT_TOTAL", xerr.CategoryConflict, 409, "refund would make total quota negative")
)
