package gid_service

import (
	"context"
	"fmt"

	pb "github.com/servekit/gid-service/gen/gid/v1"
	gidservice "github.com/servekit/gid-service/pkg"
)

type grpcGID struct {
	client *gidservice.Client
}

// NewGRPC dials gid-service at target and returns a GIDService over gRPC.
func NewGRPC(target string) (GIDService, error) {
	client, err := gidservice.NewClient(target)
	if err != nil {
		return nil, fmt.Errorf("dial gid-service: %w", err)
	}
	return &grpcGID{client: client}, nil
}

func (g *grpcGID) NextID(ctx context.Context) (int64, error) {
	resp, err := g.client.NextID(ctx, &pb.NextIDRequest{})
	if err != nil {
		return 0, err
	}
	return resp.Id, nil
}

// Close closes the underlying gRPC connection.
func (g *grpcGID) Close() error {
	return g.client.Close()
}
