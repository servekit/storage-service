package pkg

import (
	"context"

	"github.com/servekit/storage-service/internal/appauth"
)

// WithApp returns a context carrying the app credentials as gRPC metadata
// (x-app-key / x-app-secret). Module-mode callers MUST wrap their context
// before invoking data-plane RPCs — the service layer authenticates every
// write against the app registry:
//
//	resp, err := st.GenerateUploadURL(storageservice.WithApp(ctx, key, secret), req)
//
// gRPC-mode clients get the same effect by sending the metadata on the wire
// (this helper works there too — the keys land in incoming metadata).
func WithApp(ctx context.Context, appKey, appSecret string) context.Context {
	return appauth.WithApp(ctx, appKey, appSecret)
}
