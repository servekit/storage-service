package gid_service

import (
	"context"

	gidservice "github.com/servekit/gid-service/pkg"
)

// moduleGID wraps an in-process gid-service Handler.
type moduleGID struct {
	*gidservice.Handler
	owns bool
}

// NewModule wraps a gid-service Handler as a GIDService. owns reports whether
// this wrapper owns the Handler's lifecycle: true when the caller built it
// (Close Stops it), false when it was injected by a parent that owns it (Close
// is a no-op, preventing a double-Stop with the parent's lifecycle).
func NewModule(h *gidservice.Handler, owns bool) GIDService {
	return &moduleGID{Handler: h, owns: owns}
}

func (m *moduleGID) NextID(ctx context.Context) (int64, error) {
	resp, err := m.Handler.NextID(ctx, nil)
	if err != nil {
		return 0, err
	}
	return resp.Id, nil
}

// Close stops the Handler only if this wrapper owns it (self-built). A borrowed
// (injected) Handler is left to its owner, so Close is a no-op — the parent's
// lifecycle Stops it.
func (m *moduleGID) Close() error {
	if !m.owns {
		return nil
	}
	return m.Stop()
}
