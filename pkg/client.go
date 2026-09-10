package pkg

import (
	"context"
	"fmt"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	storagev1 "github.com/servekit/api/gen/go/storage/v1"

	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client is a gRPC client for storage-service shaped like *Handler: it
// implements the generated storagev1.StorageServiceServer interface (unary
// methods without grpc.CallOption), so a consumer can hold either backend
// behind that one generated interface — module mode passes the *Handler,
// grpc mode passes the *Client — with no per-consumer adapter.
//
// The UnimplementedStorageServiceServer embed satisfies the interface's
// mustEmbed guard; every RPC below shadows it with a real delegation. When a
// new RPC is added to the proto, add its delegation here — until then grpc
// mode returns codes.Unimplemented for it.
type Client struct {
	storagev1.UnimplementedStorageServiceServer

	conn *grpc.ClientConn
	cli  storagev1.StorageServiceClient
}

// Compile-time assertion: *Client and *Handler expose the same interface.
var _ storagev1.StorageServiceServer = (*Client)(nil)

// NewClient creates a new storage service gRPC client. ForwardActorUnary
// and ForwardTenantKeyUnary are always installed so the request actor and
// the trusted tenant key (the ④ gate-injected selection) cross the service
// boundary in gRPC mode.
func NewClient(addr string, opts ...grpc.DialOption) (*Client, error) {
	dialOpts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(
			grpcx.ForwardActorUnary(),
			tenantctx.ForwardTenantKeyUnary(),
		),
	}, opts...)

	conn, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	return &Client{conn: conn, cli: storagev1.NewStorageServiceClient(conn)}, nil
}

// Close closes the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Ping delegates to the remote storage-service.
func (c *Client) Ping(ctx context.Context, in *emptypb.Empty) (*commonv1.Pong, error) {
	return c.cli.Ping(ctx, in)
}

// GenerateUploadURL delegates to the remote storage-service.
func (c *Client) GenerateUploadURL(ctx context.Context, in *storagev1.GenerateUploadURLRequest) (*storagev1.GenerateUploadURLResponse, error) {
	return c.cli.GenerateUploadURL(ctx, in)
}

// GetSTSCredential delegates to the remote storage-service.
func (c *Client) GetSTSCredential(ctx context.Context, in *storagev1.GetSTSCredentialRequest) (*storagev1.GetSTSCredentialResponse, error) {
	return c.cli.GetSTSCredential(ctx, in)
}

// BatchGetSTSCredential delegates to the remote storage-service.
func (c *Client) BatchGetSTSCredential(ctx context.Context, in *storagev1.BatchGetSTSCredentialRequest) (*storagev1.BatchGetSTSCredentialResponse, error) {
	return c.cli.BatchGetSTSCredential(ctx, in)
}

// ConfirmUpload delegates to the remote storage-service.
func (c *Client) ConfirmUpload(ctx context.Context, in *storagev1.ConfirmUploadRequest) (*storagev1.ConfirmUploadResponse, error) {
	return c.cli.ConfirmUpload(ctx, in)
}

// CancelUpload delegates to the remote storage-service.
func (c *Client) CancelUpload(ctx context.Context, in *storagev1.CancelUploadRequest) (*emptypb.Empty, error) {
	return c.cli.CancelUpload(ctx, in)
}

// GenerateDownloadURL delegates to the remote storage-service.
func (c *Client) GenerateDownloadURL(ctx context.Context, in *storagev1.GenerateDownloadURLRequest) (*storagev1.GenerateDownloadURLResponse, error) {
	return c.cli.GenerateDownloadURL(ctx, in)
}

// ListMyFiles delegates to the remote storage-service.
func (c *Client) ListMyFiles(ctx context.Context, in *storagev1.ListMyFilesRequest) (*storagev1.ListMyFilesResponse, error) {
	return c.cli.ListMyFiles(ctx, in)
}

// ListMyFilesPaged delegates to the remote storage-service.
func (c *Client) ListMyFilesPaged(ctx context.Context, in *storagev1.ListMyFilesPagedRequest) (*storagev1.ListMyFilesPagedResponse, error) {
	return c.cli.ListMyFilesPaged(ctx, in)
}

// GetMyFile delegates to the remote storage-service.
func (c *Client) GetMyFile(ctx context.Context, in *storagev1.GetMyFileRequest) (*storagev1.UserFileInfo, error) {
	return c.cli.GetMyFile(ctx, in)
}

// UpdateMyFile delegates to the remote storage-service.
func (c *Client) UpdateMyFile(ctx context.Context, in *storagev1.UpdateMyFileRequest) (*storagev1.UserFileInfo, error) {
	return c.cli.UpdateMyFile(ctx, in)
}

// DeleteMyFile delegates to the remote storage-service.
func (c *Client) DeleteMyFile(ctx context.Context, in *storagev1.DeleteMyFileRequest) (*emptypb.Empty, error) {
	return c.cli.DeleteMyFile(ctx, in)
}

// BatchDeleteMyFiles delegates to the remote storage-service.
func (c *Client) BatchDeleteMyFiles(ctx context.Context, in *storagev1.BatchDeleteMyFilesRequest) (*storagev1.BatchDeleteMyFilesResponse, error) {
	return c.cli.BatchDeleteMyFiles(ctx, in)
}

// GenerateProcessURL delegates to the remote storage-service.
func (c *Client) GenerateProcessURL(ctx context.Context, in *storagev1.GenerateProcessURLRequest) (*storagev1.GenerateProcessURLResponse, error) {
	return c.cli.GenerateProcessURL(ctx, in)
}

// GenerateCDNURL delegates to the remote storage-service.
func (c *Client) GenerateCDNURL(ctx context.Context, in *storagev1.GenerateCDNURLRequest) (*storagev1.GenerateCDNURLResponse, error) {
	return c.cli.GenerateCDNURL(ctx, in)
}

// GetMyQuota delegates to the remote storage-service.
func (c *Client) GetMyQuota(ctx context.Context, in *storagev1.GetMyQuotaRequest) (*storagev1.QuotaInfo, error) {
	return c.cli.GetMyQuota(ctx, in)
}

// AdminListFiles delegates to the remote storage-service.
func (c *Client) AdminListFiles(ctx context.Context, in *storagev1.AdminListFilesRequest) (*storagev1.AdminListFilesResponse, error) {
	return c.cli.AdminListFiles(ctx, in)
}

// AdminGetFile delegates to the remote storage-service.
func (c *Client) AdminGetFile(ctx context.Context, in *storagev1.AdminGetFileRequest) (*storagev1.AdminFileInfo, error) {
	return c.cli.AdminGetFile(ctx, in)
}

// AdminDeleteFile delegates to the remote storage-service.
func (c *Client) AdminDeleteFile(ctx context.Context, in *storagev1.AdminDeleteFileRequest) (*emptypb.Empty, error) {
	return c.cli.AdminDeleteFile(ctx, in)
}

// AdminGetQuota delegates to the remote storage-service.
func (c *Client) AdminGetQuota(ctx context.Context, in *storagev1.AdminGetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return c.cli.AdminGetQuota(ctx, in)
}

// AdminSetQuota delegates to the remote storage-service.
func (c *Client) AdminSetQuota(ctx context.Context, in *storagev1.AdminSetQuotaRequest) (*storagev1.QuotaInfo, error) {
	return c.cli.AdminSetQuota(ctx, in)
}

// AdminGetStats delegates to the remote storage-service.
func (c *Client) AdminGetStats(ctx context.Context, in *storagev1.AdminGetStatsRequest) (*storagev1.AdminGetStatsResponse, error) {
	return c.cli.AdminGetStats(ctx, in)
}

// AdminListProviders delegates to the remote storage-service.
func (c *Client) AdminListProviders(ctx context.Context, in *emptypb.Empty) (*storagev1.AdminListProvidersResponse, error) {
	return c.cli.AdminListProviders(ctx, in)
}

// AdminListBuckets delegates to the remote storage-service.
func (c *Client) AdminListBuckets(ctx context.Context, in *emptypb.Empty) (*storagev1.AdminListBucketsResponse, error) {
	return c.cli.AdminListBuckets(ctx, in)
}

// AdminSoftDeleteOwnerFiles delegates to the remote storage-service.
func (c *Client) AdminSoftDeleteOwnerFiles(ctx context.Context, in *storagev1.AdminSoftDeleteOwnerFilesRequest) (*storagev1.AdminSoftDeleteOwnerFilesResponse, error) {
	return c.cli.AdminSoftDeleteOwnerFiles(ctx, in)
}

// AdminDeleteOwner delegates to the remote storage-service.
func (c *Client) AdminDeleteOwner(ctx context.Context, in *storagev1.AdminDeleteOwnerRequest) (*storagev1.AdminDeleteOwnerResponse, error) {
	return c.cli.AdminDeleteOwner(ctx, in)
}

// ListMyAuditLogs delegates to the remote storage-service.
func (c *Client) ListMyAuditLogs(ctx context.Context, in *storagev1.ListMyAuditLogsRequest) (*storagev1.ListMyAuditLogsResponse, error) {
	return c.cli.ListMyAuditLogs(ctx, in)
}

// AdminListAuditLogs delegates to the remote storage-service.
func (c *Client) AdminListAuditLogs(ctx context.Context, in *storagev1.AdminListAuditLogsRequest) (*storagev1.AdminListAuditLogsResponse, error) {
	return c.cli.AdminListAuditLogs(ctx, in)
}

// SetOwnerQuota delegates to the remote storage-service.
func (c *Client) SetOwnerQuota(ctx context.Context, in *storagev1.SetOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return c.cli.SetOwnerQuota(ctx, in)
}

// AddOwnerQuota delegates to the remote storage-service.
func (c *Client) AddOwnerQuota(ctx context.Context, in *storagev1.AddOwnerQuotaRequest) (*storagev1.QuotaInfo, error) {
	return c.cli.AddOwnerQuota(ctx, in)
}
