package pkg

import (
	"context"
	"net"
	"reflect"
	"testing"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	pb "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/go-common/grpcx/clienttest"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// smokeStub satisfies the server interface(s) with Ping overridden. The point
// of this test is the *Client's delegation routing, not the real service, so
// a stub avoids all handler fixtures (db/redis/providers).
type smokeStub struct {
	pb.UnimplementedStorageServiceServer
}

func (smokeStub) Ping(context.Context, *emptypb.Empty) (*commonv1.Pong, error) {
	return &commonv1.Pong{Status: "SERVING"}, nil
}

// TestClient_GRPCRoundTrip drives the server-shaped *Client against a real
// in-process gRPC server: Ping asserts the wire path, then EveryUnary walks
// the whole interface — a self-recursive or mis-routed delegation (the bug
// class unit tests never reach) kills the test binary here.
func TestClient_GRPCRoundTrip(t *testing.T) {
	lis, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	gs := grpc.NewServer()
	pb.RegisterStorageServiceServer(gs, smokeStub{})

	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	c, err := NewClient(lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()

	pong, err := c.Ping(ctx, &emptypb.Empty{})
	require.NoError(t, err)
	require.Equal(t, "SERVING", pong.GetStatus())

	clienttest.EveryUnary(ctx, t, c, reflect.TypeOf((*pb.StorageServiceServer)(nil)).Elem())
}
