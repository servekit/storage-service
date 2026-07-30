// Package gid_service adapts gid-service to storage-service's internal needs.
//
// The GIDService interface is internal: external callers inject the raw
// *gidservice.Handler via option.WithGIDHandler; this package wraps it into the
// clean NextID shape that storage-service's business code consumes. Building the
// raw Handler (or threading one in from a parent process) happens in the
// service root; this package only wraps.
package gid_service

import "context"

// GIDService generates globally unique IDs via gid-service. Close releases the
// underlying resource (grpc connection, or the in-process Handler) — wired to a
// lifecycle Stopper by resolveGID when this service owns the backend.
type GIDService interface {
	NextID(ctx context.Context) (int64, error)
	Close() error
}
