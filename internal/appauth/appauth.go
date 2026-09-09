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

// WithApp returns a context carrying the app credentials as BOTH incoming
// and outgoing metadata: incoming covers module-mode (in-process) calls
// where the context flows straight into the service impl; outgoing lets a
// real gRPC client forward them to a remote server.
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(KeyAppKey, appKey, KeyAppSecret, appSecret))
	return metadata.AppendToOutgoingContext(ctx, KeyAppKey, appKey, KeyAppSecret, appSecret)
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
