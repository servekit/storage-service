// Package appauth resolves the calling app identity from gRPC metadata
// (x-app-key / x-app-secret). The check runs in the service layer — not a
// gRPC interceptor — so module-mode in-process callers share the exact same
// code path: WithApp plants the credentials as incoming metadata, and the
// gRPC server path arrives with them naturally.
//
// Copied verbatim from message-service/internal/appauth (same platform
// pattern; secrets are per-service, only the key names are shared).
package appauth

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// Metadata keys carrying the app credentials.
const (
	KeyAppKey    = "x-app-key"
	KeyAppSecret = "x-app-secret"
)

// WithApp returns a context carrying the app credentials as incoming
// metadata. Module-mode callers wrap their context before invoking the
// storage Service; gRPC-mode clients get the same effect for free when the
// credentials travel as metadata (or call this on the outgoing side).
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(KeyAppKey, appKey, KeyAppSecret, appSecret))
}

// Credentials returns the (app_key, app_secret) pair from incoming
// metadata. ok is false when either key is missing.
func Credentials(ctx context.Context) (appKey, appSecret string, ok bool) {
	md, have := metadata.FromIncomingContext(ctx)
	if !have {
		return "", "", false
	}
	keys := md.Get(KeyAppKey)
	secrets := md.Get(KeyAppSecret)
	if len(keys) == 0 || len(secrets) == 0 || keys[0] == "" || secrets[0] == "" {
		return "", "", false
	}
	return keys[0], secrets[0], true
}
