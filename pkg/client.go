package pkg

import (
	storagev1 "github.com/servekit/storage-service/gen/storage/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is a gRPC client for the storage service.
type Client struct {
	conn *grpc.ClientConn
	storagev1.StorageServiceClient
}

// NewClient creates a new storage service gRPC client.
func NewClient(addr string, opts ...grpc.DialOption) (*Client, error) {
	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, opts...)

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, err
	}

	return &Client{
		conn:                 conn,
		StorageServiceClient: storagev1.NewStorageServiceClient(conn),
	}, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}
