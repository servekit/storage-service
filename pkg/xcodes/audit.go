package xcodes

import "github.com/servekit/go-common/xerr"

// Audit error codes.
var (
	ErrAuditLogNotFound = xerr.New("AUDIT_LOG_NOT_FOUND", xerr.CategoryNotFound, 404, "audit log not found")
)
