package pkg

import (
	"errors"
	"fmt"

	"buf.build/go/protovalidate"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/signalx"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"

	storagev1 "github.com/servekit/storage-service/gen/storage/v1"
	"github.com/servekit/storage-service/internal/service"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/handler"
	"github.com/servekit/storage-service/pkg/option"
)

// Server wraps a gRPC server for the storage service.
type Server struct {
	grpcSrv *grpcx.Server
	hdl     *handler.Handler
}

// ServerOption configures a Server instance.
type ServerOption func(*serverOptions)

type serverOptions struct {
	serviceOpts []option.Option
}

// Compile-time assertion that *Server satisfies signalx.Service.
var _ signalx.Service = (*Server)(nil)

// WithServiceOptions forwards options to the service layer.
func WithServiceOptions(opts ...option.Option) ServerOption {
	return func(o *serverOptions) { o.serviceOpts = append(o.serviceOpts, opts...) }
}

// NewServer creates a Server with all dependencies.
//
// SECURITY: this constructor wires only ErrorInterceptor (xerr → gRPC status
// mapping) and protovalidate (request validation). It does NOT install an
// authentication/authorization interceptor — every RPC, including admin*
// management RPCs (AdminDeleteOwner, AdminDeleteFile, AdminSetQuota, …) and
// privileged business RPCs (SetOwnerQuota, AddOwnerQuota), is reachable
// anonymously. Callers also self-declare their identity via the Owner proto
// field on each request, which the service trusts without verification.
//
// This is acceptable ONLY when the service sits behind a trusted boundary
// that performs authentication (API gateway, service mesh, sidecar) or when
// it is linked into a host process as a Go module and the host enforces
// auth. Exposing :9000 (gRPC) or :8080 (gateway) directly to untrusted
// networks lets any caller delete owners, read all files, or change quotas.
// Add an auth interceptor here before deploying outside a trusted boundary.
func NewServer(cfg *config.Config, opts ...ServerOption) (*Server, error) {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}

	svc, err := service.New(cfg, o.serviceOpts...)
	if err != nil {
		return nil, err
	}
	hdl := handler.FromService(svc)

	validator, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("init protovalidate: %w", err)
	}

	grpcSrv := grpcx.New(
		&grpcx.ServerConfig{
			GRPCAddr:    cfg.Server.GRPCAddr,
			GatewayAddr: cfg.Server.HTTPAddr,
		},
		func(gs *grpc.Server) {
			storagev1.RegisterStorageServiceServer(gs, hdl)
		},
		storagev1.RegisterStorageServiceHandlerFromEndpoint,
		grpcx.ErrorInterceptor,
		protovalidate_middleware.UnaryServerInterceptor(validator),
	)

	return &Server{grpcSrv: grpcSrv, hdl: hdl}, nil
}

// Start starts service internals and the gRPC + HTTP gateway without blocking.
// If grpcSrv.Start fails, hdl.Stop is called to roll back partial startup.
func (s *Server) Start() error {
	if err := s.hdl.Start(); err != nil {
		return err
	}
	if err := s.grpcSrv.Start(); err != nil {
		return errors.Join(err, s.hdl.Stop())
	}
	return nil
}

// Stop gracefully stops the gRPC + HTTP gateway and service internals.
// Errors from each component are aggregated via errors.Join.
func (s *Server) Stop() error {
	return errors.Join(s.grpcSrv.Stop(), s.hdl.Stop())
}
