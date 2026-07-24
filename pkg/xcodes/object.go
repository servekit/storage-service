package xcodes

import "github.com/servekit/go-common/xerr"

// Object error codes.
var (
	ErrObjectNotActive            = xerr.New("OBJECT_NOT_ACTIVE", xerr.CategoryNotFound, 404, "object not found or deleted")
	ErrObjectInsufficientRefCount = xerr.New("OBJECT_INSUFFICIENT_REF_COUNT", xerr.CategoryConflict, 409, "insufficient reference count")
	ErrObjectNotSoftDeleted       = xerr.New("OBJECT_NOT_SOFT_DELETED", xerr.CategoryConflict, 409, "object is not soft-deleted")
)
