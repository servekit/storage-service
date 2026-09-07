// Platform admin RPCs: provider / bucket / settings CRUD with deletion and
// rebind guards, audit logging, and immediate live-registry rebuild.
package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/service/conv"
	"github.com/servekit/storage-service/internal/service/platform"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
)

// refreshPlatform reloads the DB platform tables into the live registry.
// Failure is logged, not returned: the write committed, and the cron
// refresh converges.
func (s *Service) refreshPlatform(ctx context.Context) {
	if err := platform.LoadAndRebuild(ctx, s.db, s.registry, "", ""); err != nil {
		slog.Error("admin: platform refresh after mutation (cron will converge)", "error", err)
	}
}

// --- Providers ---

// AdminCreateProvider adds a provider to the live registry.
func (s *Service) AdminCreateProvider(ctx context.Context, req *storagev1.AdminCreateProviderRequest) (*storagev1.AdminCreateProviderResponse, error) {
	if _, err := dal.GetProviderByName(ctx, s.db, req.GetName()); err == nil {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("provider %q already exists", req.GetName()))
	}
	if !vendorSupported(req.GetVendor()) {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("unsupported vendor %s", req.GetVendor()))
	}
	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	p := &models.StorageProvider{
		ID:        id,
		Name:      req.GetName(),
		Vendor:    int32(req.GetVendor()),
		Endpoint:  req.GetEndpoint(),
		Region:    req.GetRegion(),
		AccessKey: req.GetAccessKey(),
		SecretKey: req.GetSecretKey(),
		RoleARN:   req.GetRoleArn(),
		DomainID:  req.GetDomainId(),
	}
	if err := dal.CreateProvider(ctx, s.db, p); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_CREATE_PROVIDER,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_PROVIDER, p.ID, nil, providerSnapshot(p))
	s.refreshPlatform(ctx)
	return &storagev1.AdminCreateProviderResponse{Provider: providerToProto(p, 0)}, nil
}

// AdminUpdateProvider edits a provider; credential fields are
// replace-on-present, absent optional fields keep their values.
func (s *Service) AdminUpdateProvider(ctx context.Context, req *storagev1.AdminUpdateProviderRequest) (*storagev1.AdminUpdateProviderResponse, error) {
	p, err := dal.GetProviderByName(ctx, s.db, req.GetName())
	if err != nil {
		return nil, err
	}
	before := providerSnapshot(p)
	if req.Endpoint != nil {
		p.Endpoint = req.GetEndpoint()
	}
	if req.Region != nil {
		p.Region = req.GetRegion()
	}
	if req.AccessKey != nil {
		p.AccessKey = req.GetAccessKey()
	}
	if req.SecretKey != nil {
		p.SecretKey = req.GetSecretKey()
	}
	if req.RoleArn != nil {
		p.RoleARN = req.GetRoleArn()
	}
	if req.DomainId != nil {
		p.DomainID = req.GetDomainId()
	}
	if req.Disabled != nil {
		p.Disabled = req.GetDisabled()
	}
	if err := dal.UpdateProvider(ctx, s.db, p); err != nil {
		return nil, err
	}
	count, _ := dal.CountBucketsForProvider(ctx, s.db, p.ID)
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_UPDATE_PROVIDER,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_PROVIDER, p.ID, before, providerSnapshot(p))
	s.refreshPlatform(ctx)
	return &storagev1.AdminUpdateProviderResponse{Provider: providerToProto(p, int32(count))}, nil
}

// AdminDeleteProvider removes a provider; rejected while buckets are still
// bound to it.
func (s *Service) AdminDeleteProvider(ctx context.Context, req *storagev1.AdminDeleteProviderRequest) (*emptypb.Empty, error) {
	p, err := dal.GetProviderByName(ctx, s.db, req.GetName())
	if err != nil {
		return nil, err
	}
	count, err := dal.CountBucketsForProvider(ctx, s.db, p.ID)
	if err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, xcodes.ErrProviderHasBuckets.New(fmt.Sprintf(
			"provider %q still has %d bound bucket(s); rebind or delete them first", p.Name, count))
	}
	if err := dal.DeleteProvider(ctx, s.db, p.ID); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_PROVIDER,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_PROVIDER, p.ID, providerSnapshot(p), nil)
	s.refreshPlatform(ctx)
	return &emptypb.Empty{}, nil
}

// --- Buckets ---

// AdminUpsertBucket creates or fully replaces a bucket binding by name.
// Re-binding a bucket that still has objects to a different provider is
// rejected — object rows carry (vendor, bucket) and would dangle.
func (s *Service) AdminUpsertBucket(ctx context.Context, req *storagev1.AdminUpsertBucketRequest) (*storagev1.AdminUpsertBucketResponse, error) {
	provider, err := dal.GetProviderByName(ctx, s.db, req.GetProvider())
	if err != nil {
		return nil, err
	}
	if !vendorSupported(storagev1.Vendor(provider.Vendor)) {
		return nil, xcodes.ErrInternal.New(fmt.Sprintf("provider row %q carries unknown vendor %d", provider.Name, provider.Vendor))
	}
	acl := aclFromProto(req.GetAcl())
	if acl == "" {
		return nil, xcodes.ErrBadRequest.New("acl is required")
	}
	if err := validateBucketCDNShape(storagev1.Vendor(provider.Vendor), req.GetCdn()); err != nil {
		return nil, err
	}

	existing, getErr := dal.GetBucket(ctx, s.db, req.GetName())
	if getErr != nil {
		if !errors.Is(getErr, xcodes.ErrBucketNotFound.New()) {
			return nil, getErr
		}
		// create
		id, err := gidservice.NextID(ctx, s.gid)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		b := platform.BucketRowFromConfig(req.GetName(), provider.ID, req.GetKeyPrefix(), acl, req.GetCdn())
		b.ID = id
		if err := dal.CreateBucket(ctx, s.db, b); err != nil {
			return nil, err
		}
		s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_UPSERT_BUCKET,
			storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_BUCKET, b.ID, nil, bucketSnapshot(b))
		s.refreshPlatform(ctx)
		return &storagev1.AdminUpsertBucketResponse{Bucket: s.bucketToProto(ctx, b, provider)}, nil
	}

	// update (full replace)
	if existing.ProviderID != provider.ID {
		n, err := dal.CountObjectsInBucket(ctx, s.db, existing.Name)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, xcodes.ErrBucketHasObjects.New(fmt.Sprintf(
				"bucket %q still has %d object(s); re-binding to another provider would dangle them", existing.Name, n))
		}
	}
	before := bucketSnapshot(existing)
	existing.ProviderID = provider.ID
	existing.KeyPrefix = req.GetKeyPrefix()
	existing.ACL = acl
	existing.CDNDomain, existing.CDNAuthKey, existing.CDNKeyPairID = "", "", ""
	if cdn := req.GetCdn(); cdn != nil {
		existing.CDNDomain = cdn.GetDomain()
		existing.CDNAuthKey = cdn.GetAuthKey()
		existing.CDNKeyPairID = cdn.GetKeyPairId()
	}
	if err := dal.UpdateBucket(ctx, s.db, existing); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_UPSERT_BUCKET,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_BUCKET, existing.ID, before, bucketSnapshot(existing))
	s.refreshPlatform(ctx)
	return &storagev1.AdminUpsertBucketResponse{Bucket: s.bucketToProto(ctx, existing, provider)}, nil
}

// AdminDeleteBucket removes a bucket binding; rejected while the bucket
// still has objects.
func (s *Service) AdminDeleteBucket(ctx context.Context, req *storagev1.AdminDeleteBucketRequest) (*emptypb.Empty, error) {
	b, err := dal.GetBucket(ctx, s.db, req.GetName())
	if err != nil {
		return nil, err
	}
	n, err := dal.CountObjectsInBucket(ctx, s.db, b.Name)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, xcodes.ErrBucketHasObjects.New(fmt.Sprintf(
			"bucket %q still has %d object(s); deleting the binding would dangle them", b.Name, n))
	}
	if err := dal.DeleteBucket(ctx, s.db, b.ID); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_BUCKET,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_BUCKET, b.ID, bucketSnapshot(b), nil)
	s.refreshPlatform(ctx)
	return &emptypb.Empty{}, nil
}

// --- Settings ---

// AdminGetSettings returns the runtime settings row.
func (s *Service) AdminGetSettings(ctx context.Context, _ *storagev1.AdminGetSettingsRequest) (*storagev1.AdminGetSettingsResponse, error) {
	settings, err := dal.GetSettings(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return &storagev1.AdminGetSettingsResponse{Settings: settingsToProto(settings)}, nil
}

// AdminUpdateSettings updates default/public bucket names; referenced
// buckets must exist.
func (s *Service) AdminUpdateSettings(ctx context.Context, req *storagev1.AdminUpdateSettingsRequest) (*storagev1.AdminUpdateSettingsResponse, error) {
	settings, err := dal.GetSettings(ctx, s.db)
	if err != nil {
		return nil, err
	}
	before := settingsToProto(settings)
	if req.DefaultBucket != nil {
		if req.GetDefaultBucket() != "" {
			if _, err := dal.GetBucket(ctx, s.db, req.GetDefaultBucket()); err != nil {
				return nil, err
			}
		}
		settings.DefaultBucket = req.GetDefaultBucket()
	}
	if req.PublicBucket != nil {
		if req.GetPublicBucket() != "" {
			if _, err := dal.GetBucket(ctx, s.db, req.GetPublicBucket()); err != nil {
				return nil, err
			}
		}
		settings.PublicBucket = req.GetPublicBucket()
	}
	if err := dal.UpdateSettings(ctx, s.db, settings); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_UPDATE_SETTINGS,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_SETTINGS, settings.ID,
		map[string]any{"default_bucket": before.GetDefaultBucket(), "public_bucket": before.GetPublicBucket()},
		map[string]any{"default_bucket": settings.DefaultBucket, "public_bucket": settings.PublicBucket})
	s.refreshPlatform(ctx)
	return &storagev1.AdminUpdateSettingsResponse{Settings: settingsToProto(settings)}, nil
}

// --- helpers ---

// ToVendor renders the provider row's vendor enum (helper kept on the model
// to keep admin/platform conversions in one place).
func vendorSupported(v storagev1.Vendor) bool {
	switch v {
	case storagev1.Vendor_VENDOR_ALIYUN_OSS,
		storagev1.Vendor_VENDOR_AWS_S3,
		storagev1.Vendor_VENDOR_S3_COMPATIBLE,
		storagev1.Vendor_VENDOR_TENCENT_COS,
		storagev1.Vendor_VENDOR_HUAWEI_OBS,
		storagev1.Vendor_VENDOR_VOLCENGINE_TOS:
		return true
	default:
		return false
	}
}

// aclFromProto converts the proto BucketACL to the config-string vocabulary.
func aclFromProto(acl storagev1.BucketACL) string {
	switch acl {
	case storagev1.BucketACL_BUCKET_ACL_PRIVATE:
		return "private"
	case storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ:
		return "public_read"
	case storagev1.BucketACL_BUCKET_ACL_PUBLIC_READ_WRITE:
		return "public_read_write"
	default:
		return ""
	}
}

// validateBucketCDNShape enforces the vendor-driven KeyPairID contract
// (required for the CloudFront path, rejected elsewhere) — mirrors the
// config-time validateBucketCDN rule.
func validateBucketCDNShape(vendor storagev1.Vendor, cdn *storagev1.CDNConfig) error {
	if cdn == nil {
		return nil
	}
	if cdn.GetDomain() == "" {
		return xcodes.ErrBadRequest.New("cdn.domain is required when cdn is set")
	}
	switch vendor {
	case storagev1.Vendor_VENDOR_AWS_S3, storagev1.Vendor_VENDOR_S3_COMPATIBLE:
		if cdn.GetKeyPairId() == "" {
			return xcodes.ErrBadRequest.New("cdn.key_pair_id is required for CloudFront-backed vendors")
		}
	default:
		if cdn.GetKeyPairId() != "" {
			return xcodes.ErrBadRequest.New("cdn.key_pair_id is only valid for CloudFront-backed vendors")
		}
	}
	return nil
}

func providerToProto(p *models.StorageProvider, bucketCount int32) *storagev1.ProviderInfo {
	return &storagev1.ProviderInfo{
		Name:        p.Name,
		Vendor:      storagev1.Vendor(p.Vendor),
		Endpoint:    p.Endpoint,
		Region:      p.Region,
		Disabled:    p.Disabled,
		StsEnabled:  p.RoleARN != "",
		BucketCount: bucketCount,
	}
}

func (s *Service) bucketToProto(_ context.Context, b *models.StorageBucket, provider *models.StorageProvider) *storagev1.BucketInfo {
	info := &storagev1.BucketInfo{
		Name:      b.Name,
		Provider:  provider.Name,
		KeyPrefix: b.KeyPrefix,
		Acl:       conv.ACLToProto(b.ACL),
		Vendor:    storagev1.Vendor(provider.Vendor),
	}
	if b.CDNDomain != "" {
		info.Cdn = &storagev1.CDNConfig{
			Domain:    b.CDNDomain,
			AuthKey:   b.CDNAuthKey,
			KeyPairId: b.CDNKeyPairID,
		}
	}
	return info
}

func settingsToProto(s *models.StorageSetting) *storagev1.StorageSettings {
	return &storagev1.StorageSettings{
		DefaultBucket: s.DefaultBucket,
		PublicBucket:  s.PublicBucket,
	}
}

func providerSnapshot(p *models.StorageProvider) map[string]any {
	return map[string]any{
		"name": p.Name, "vendor": platform.VendorEnumToName(storagev1.Vendor(p.Vendor)),
		"endpoint": p.Endpoint, "region": p.Region,
		"role_arn": p.RoleARN, "domain_id": p.DomainID, "disabled": p.Disabled,
	}
}

func bucketSnapshot(b *models.StorageBucket) map[string]any {
	return map[string]any{
		"name": b.Name, "provider_id": b.ProviderID,
		"key_prefix": b.KeyPrefix, "acl": b.ACL, "cdn_domain": b.CDNDomain,
	}
}

// auditPlatform records a platform mutation event (system actor — these
// RPCs carry no owner identity).
func (s *Service) auditPlatform(ctx context.Context, action storagev1.AuditAction,
	targetType storagev1.AuditLogTargetType, targetID int64, before, after map[string]any) {
	s.audit.RecordOutcome(ctx, audit.Event{
		Action:     action,
		OwnerType:  int32(storagev1.OwnerType_OWNER_TYPE_SYSTEM),
		TargetType: targetType,
		TargetID:   targetID,
		Before:     before,
		After:      after,
	}, nil)
}
