// Package handler is the thin gRPC shell for the storage service. Each method
// is a one-line delegation to internal/service.StorageService — handler holds
// no business logic and performs no protocol conversion.
package handler

import (
	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/service"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/option"

	"github.com/servekit/go-common/signalx"
)

// Handler implements storagev1.StorageServiceServer by delegating every RPC to
// the underlying *service.StorageService. It also satisfies signalx.Service so
// in-process module callers can manage lifecycle on the same object.
type Handler struct {
	storagev1.UnimplementedStorageServiceServer
	svc *service.StorageService
}

// Compile-time assertion that *Handler satisfies signalx.Service.
var _ signalx.Service = (*Handler)(nil)

// New constructs a Handler wrapping a freshly-built service. The service is
// created from cfg + opts; Handler owns no resources of its own — Start/Stop
// forward to the underlying service.
func New(cfg *config.Config, opts ...option.Option) (*Handler, error) {
	svc, err := service.New(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &Handler{svc: svc}, nil
}

// FromService wraps an existing *service.StorageService. Used by pkg.Server,
// which constructs the service separately for clearer error messages.
func FromService(svc *service.StorageService) *Handler {
	return &Handler{svc: svc}
}

// Start forwards to the underlying service so background goroutines (cron GC)
// begin running. No-op if the service has no starters.
func (h *Handler) Start() error { return h.svc.Start() }

// Stop forwards to the underlying service so owned resources are released.
func (h *Handler) Stop() error { return h.svc.Stop() }
