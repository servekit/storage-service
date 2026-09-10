// Actor-scope guard for the cross-owner audit surface (tenant platform
// phase ④ T5 — server-side closure). AdminListAuditLogs reads across every
// owner — a cross-tenant platform surface with no tenant dimension, so it
// admits only the PLATFORM cross-view: an injected tenant key is refused
// (the caller is pinned to a tenant), and a caller with neither identity
// fails closed. Mirrors admin.requirePlatformScope in this repo.
package audit

import (
	"context"

	userv1 "github.com/servekit/api/gen/go/user/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"

	"github.com/servekit/storage-service/pkg/xcodes"
)

// requirePlatformScope admits only the PLATFORM cross-view (no injected
// tenant key, verified platform actor); everything else fails closed.
func requirePlatformScope(ctx context.Context) error {
	if _, ok := tenantctx.TenantKeyFromCtx(ctx); ok {
		return xcodes.ErrForbidden.New("platform operators only")
	}
	actor, err := grpcx.MustActorFromCtx(ctx)
	if err != nil {
		return xcodes.ErrUnauthorized.New()
	}
	if userv1.UserType(actor.GetUserType()) != userv1.UserType_USER_TYPE_PLATFORM {
		return xcodes.ErrForbidden.New("platform operators only")
	}
	return nil
}
