package pkg

import (
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/handler"
	"github.com/servekit/storage-service/pkg/option"
)

// NewModule creates a Handler for in-process use. The Handler satisfies both
// storagev1.StorageServiceServer (call RPC methods directly) and signalx.Service
// (manage lifecycle). Callers that inject resources via options own those
// resources' lifecycle; Handler.Stop only releases resources it created.
func NewModule(cfg *config.Config, opts ...option.Option) (*handler.Handler, error) {
	return handler.New(cfg, opts...)
}
