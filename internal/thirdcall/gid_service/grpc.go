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

// NewGRPC creates a GIDService backed by a gRPC connection to gid-service.
func NewGRPC(target string) (*grpcGID, error) {
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

func (g *grpcGID) Close() error {
	return g.client.Close()
}
