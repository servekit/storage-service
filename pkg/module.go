package pkg

import (
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/handler"
	"github.com/servekit/storage-service/pkg/option"

	"gorm.io/gorm"
)

// NewModule creates a Handler for in-process use. The Handler satisfies both
// storagev1.StorageServiceServer (call RPC methods directly) and signalx.Service
// (manage lifecycle). Callers that inject resources via options own those
// resources' lifecycle; Handler.Stop only releases resources it created.
func NewModule(cfg *config.Config, opts ...option.Option) (*handler.Handler, error) {
	return handler.New(cfg, opts...)
}

// Migrate applies the current schema (GORM AutoMigrate) to db. It re-exports
// handler.Migrate so embedders and the `migrate` subcommand share one entry
// point (where `storage` is the embedder's import alias for this package):
//
//	storage.Migrate(parentDB)                                     // before NewModule
//	hdl, err := storage.NewModule(cfg, option.WithDB(parentDB))
func Migrate(db *gorm.DB) error {
	return handler.Migrate(db)
}
