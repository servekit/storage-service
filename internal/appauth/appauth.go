// Package appauth carries calling-app credentials through gRPC metadata.
// Phase ③ dual-stack window: the trusted tenant identity (x-tenant-key,
// injected by the portal proxy — internal-network trust) rides alongside the
// legacy per-app credentials (x-app-key / x-app-secret); Resolve applies the
// D-③1 precedence (tenant key authoritative, legacy pair only when the
// trusted key is absent). Verification happens in the service layer — not an
// interceptor — so module-mode in-process callers share the same path as
// gRPC clients: WithApp / WithTenant plant the credentials as incoming
// metadata, and the gRPC server path arrives with them naturally. The legacy
// half is deleted wholesale when the window closes (phase ④).
package appauth

import (
	"context"

	"google.golang.org/grpc/metadata"

	"github.com/servekit/go-common/dualauth"
	"github.com/servekit/go-common/tenantctx"
)

// Metadata keys carrying the legacy app credentials.
const (
	KeyAppKey    = "x-app-key"
	KeyAppSecret = "x-app-secret"
)

// WithApp returns a context carrying the app credentials as BOTH incoming
// and outgoing metadata: incoming covers module-mode (in-process) calls where
// the context flows straight into the service impl; outgoing lets a real
// gRPC client forward them to a remote server.
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(KeyAppKey, appKey, KeyAppSecret, appSecret))
	return metadata.AppendToOutgoingContext(ctx, KeyAppKey, appKey, KeyAppSecret, appSecret)
}

// WithTenant plants the trusted tenant key on ctx as BOTH incoming and
// outgoing metadata — the module-mode mirror of what the portal proxy does
// on the wire (tenantctx.HeaderTenantKey). Merge semantics match WithApp:
// existing incoming keys (client-info, legacy credentials) survive.
func WithTenant(ctx context.Context, tenantKey string) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		md = md.Copy()
		md.Set(tenantctx.HeaderTenantKey, tenantKey)
		ctx = metadata.NewIncomingContext(ctx, md)
	} else {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(tenantctx.HeaderTenantKey, tenantKey))
	}
	return metadata.AppendToOutgoingContext(ctx, tenantctx.HeaderTenantKey, tenantKey)
}

// Resolve applies the D-③1 precedence rule to the ctx's INCOMING metadata
// and classifies the caller's credential stack:
//
//   - SourceTrusted: x-tenant-key present — authoritative; any legacy
//     credentials riding the same metadata are discarded (prevents smuggled
//     app creds from impersonating another tenant through the proxy). No
//     verification here: the trust basis is the internal-network invariant
//     behind the two doors (portal proxy, testkit gateway).
//   - SourceLegacy: only a complete x-app-key + x-app-secret pair — the
//     pair is returned for the caller's per-service validation.
//   - SourceNone: no usable credentials — callers must fail closed.
func Resolve(ctx context.Context) (tenantKey, appKey, appSecret string, source dualauth.Source) {
	md, _ := metadata.FromIncomingContext(ctx)
	return dualauth.Resolve(md)
}
