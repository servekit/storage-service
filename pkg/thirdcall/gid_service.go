package thirdcall

import (
	"context"

	"github.com/servekit/storage-service/internal/thirdcall/gid_service"
	"github.com/servekit/storage-service/pkg/config"
)

// GIDService generates globally unique IDs via gid-service.
type GIDService interface {
	NextID(ctx context.Context) (int64, error)
}

// NewGIDService creates a GIDService based on config mode.
func NewGIDService(cfg *config.RemoteServiceConfig[*config.SnowflakeConfig]) (GIDService, error) {
	switch cfg.Mode {
	case "grpc":
		return gid_service.NewGRPC(cfg.Target)
	default:
		return gid_service.NewModule(cfg.Config)
	}
}
