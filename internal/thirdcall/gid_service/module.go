package gid_service

import (
	"context"

	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/servekit/storage-service/pkg/config"
)

type moduleGID struct {
	*gidservice.Handler
}

// NewModule creates a GIDService backed by an in-process snowflake generator.
func NewModule(cfg *config.SnowflakeConfig) (*moduleGID, error) {
	svc, err := gidservice.NewModule(&gidconfig.Config{
		Snowflake: &gidconfig.SnowflakeConfig{
			MachineID: cfg.MachineID,
			StartTime: cfg.StartTime,
		},
	})
	if err != nil {
		return nil, err
	}
	return &moduleGID{Handler: svc}, nil
}

func (m *moduleGID) NextID(ctx context.Context) (int64, error) {
	resp, err := m.Handler.NextID(ctx, nil)
	if err != nil {
		return 0, err
	}
	return resp.Id, nil
}
