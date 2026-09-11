// Tenant-config admin RPCs for the storage platform (phase ④ T6 rename of
// the apps surface): one config row per tenant; the row keeps its internal
// calling-application identity — app_key/key_prefix are immutable, the
// credential column was retired with the ④ window close (spec §9.1.3; the
// data plane authenticates via the trusted x-tenant-key), deletion is soft
// and takes effect on the next registry refresh (which runs immediately
// after each mutation below).
package admin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
)

// AdminEnsureTenantConfig idempotently provisions the tenant's config row
// (phase ④ T6 rename of AdminCreateApp — the name now tells the truth).
// When the tenant already has a live row it is returned as-is
// (key_prefix/bucket/name only apply to a fresh create — the tenant-key
// lookup prefers the mapping and falls back to the app_key literal for
// pre-backfill rows); otherwise a row is created with the app identity
// minted server-side ("sto_" + 8 base36, collision-checked) and
// tenant_key empty-on-the-cross-view defaulting to the minted literal
// (the phase ③ legacy→tenant fallback). A scoped caller is clamped to the
// injected key, so their ensure is the read-back of their own row.
func (s *Service) AdminEnsureTenantConfig(ctx context.Context, req *storagev1.AdminEnsureTenantConfigRequest) (*storagev1.AdminEnsureTenantConfigResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	// Resolve the effective tenant first: a scoped caller (or an explicit
	// cross-view target) may already own a row — that IS the ensure answer.
	targetTenant := req.GetTenantKey()
	if scope != "" {
		targetTenant = scope
	}
	if targetTenant != "" {
		if existing, err := dal.GetAppForTenant(ctx, s.db, targetTenant); err != nil {
			return nil, err
		} else if existing != nil {
			return &storagev1.AdminEnsureTenantConfigResponse{Config: appToProto(existing)}, nil
		}
	}
	if req.GetKeyPrefix() == "" {
		return nil, xcodes.ErrBadRequest.New("key_prefix is required")
	}
	appKey, err := s.mintUniqueAppKey(ctx)
	if err != nil {
		return nil, err
	}
	if n, err := dal.CountAppsByKeyPrefix(ctx, s.db, req.GetKeyPrefix()); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, xcodes.ErrPrefixTaken.New(fmt.Sprintf("key_prefix %q already in use", req.GetKeyPrefix()))
	}
	// Fresh row's tenant stamp: clamped to the injected key for scoped
	// callers; explicit on the cross-view; the minted app_key literal as the
	// window fallback (duplicate mappings still answer the friendly error).
	tenantKey := clampTenantKey(scope, req.GetTenantKey(), appKey)
	if n, err := dal.CountAppsByTenantKey(ctx, s.db, tenantKey); err != nil {
		return nil, err
	} else if n > 0 {
		return nil, xcodes.ErrAppExists.New(fmt.Sprintf("tenant_key %q already mapped to another app", tenantKey))
	}
	if req.GetBucketId() != 0 {
		if _, err := dal.GetBucketByID(ctx, s.db, req.GetBucketId()); err != nil {
			return nil, err
		}
	}

	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrapf(err, "generate app id")
	}
	app := &models.StorageApp{
		ID:        id,
		AppKey:    appKey,
		Name:      req.GetName(),
		KeyPrefix: req.GetKeyPrefix(),
		TenantKey: models.TenantKeyPtr(tenantKey),
		BucketID:  req.GetBucketId(),
	}
	if err := dal.CreateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_CREATE_APP,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_APP, app.ID, nil, appSnapshot(app))
	s.refreshPlatform(ctx)
	return &storagev1.AdminEnsureTenantConfigResponse{Config: appToProto(app)}, nil
}

// AdminGetTenantConfig returns the tenant's config row (tenant_key
// selector; legacy pre-backfill rows resolve through the app_key-literal
// fallback). A scoped caller sees only their own row (foreign rows answer
// not-found — anti-enumeration).
func (s *Service) AdminGetTenantConfig(ctx context.Context, req *storagev1.AdminGetTenantConfigRequest) (*storagev1.AdminGetTenantConfigResponse, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	return &storagev1.AdminGetTenantConfigResponse{Config: appToProto(app)}, nil
}

// AdminUpdateTenantConfig edits mutable fields. The row's identity and
// key_prefix are immutable (objects already live under the prefix); bucket
// rebinding only affects new uploads. Ownership-checked against the
// caller's scope.
func (s *Service) AdminUpdateTenantConfig(ctx context.Context, req *storagev1.AdminUpdateTenantConfigRequest) (*storagev1.AdminUpdateTenantConfigResponse, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	before := appSnapshot(app)
	if req.Name != nil {
		app.Name = *req.Name
	}
	if req.Disabled != nil {
		app.Disabled = *req.Disabled
	}
	if req.BucketId != nil {
		if *req.BucketId != 0 {
			if _, err := dal.GetBucketByID(ctx, s.db, *req.BucketId); err != nil {
				return nil, err
			}
		}
		app.BucketID = *req.BucketId
	}
	if err := dal.UpdateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_UPDATE_APP,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_APP, app.ID, before, appSnapshot(app))
	s.refreshPlatform(ctx)
	return &storagev1.AdminUpdateTenantConfigResponse{Config: appToProto(app)}, nil
}

// AdminRotateTenantConfigSecret is retired: the app_secret column was
// dropped when the ④ window closed (spec §9.1.3) — config rows carry no
// credential to rotate. Ownership-checked against the caller's scope before
// refusing, so a foreign row still answers not-found.
func (s *Service) AdminRotateTenantConfigSecret(ctx context.Context, req *storagev1.AdminRotateTenantConfigSecretRequest) (*storagev1.AdminRotateTenantConfigSecretResponse, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	_ = app
	return nil, xcodes.ErrSecretRetired.New("app_secret was retired with the ④ window close; the data plane authenticates via the trusted x-tenant-key")
}

// AdminListTenantConfigs lists the live config rows in the caller's scope:
// for an injected key only the tenant's row; the cross-view the whole
// registry (one row per tenant, low cardinality, no paging).
func (s *Service) AdminListTenantConfigs(ctx context.Context, _ *storagev1.AdminListTenantConfigsRequest) (*storagev1.AdminListTenantConfigsResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := dal.ListApps(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]*storagev1.StorageTenantConfigInfo, 0, len(apps))
	for _, a := range apps {
		if !visibleInTenant(scope, a) {
			continue
		}
		out = append(out, appToProto(a))
	}
	return &storagev1.AdminListTenantConfigsResponse{Configs: out}, nil
}

// AdminDeleteTenantConfig soft-deletes the tenant's config row. Data-plane
// calls fail on the next registry refresh (immediate here); existing
// objects/files stay readable — their keys are stored on the rows.
// Ownership-checked against the caller's scope.
func (s *Service) AdminDeleteTenantConfig(ctx context.Context, req *storagev1.AdminDeleteTenantConfigRequest) (*emptypb.Empty, error) {
	app, err := s.configForTenantScoped(ctx, req.GetTenantKey())
	if err != nil {
		return nil, err
	}
	if err := dal.DeleteApp(ctx, s.db, app.ID); err != nil {
		return nil, err
	}
	s.auditPlatform(ctx, storagev1.AuditAction_AUDIT_ACTION_ADMIN_DELETE_APP,
		storagev1.AuditLogTargetType_AUDIT_LOG_TARGET_TYPE_APP, app.ID, appSnapshot(app), nil)
	s.refreshPlatform(ctx)
	return &emptypb.Empty{}, nil
}

// configForTenantScoped is the resolver every tenant-config RPC funnels
// through (the T5 appByAppKeyScoped shape, re-keyed to the tenant selector
// in T6): fail closed on a caller with no trusted identity, resolve the
// row by tenant_key (legacy NULL-column rows fall back to the app_key
// literal), then enforce the scope AFTER the row load — a foreign tenant's
// row answers the same not-found a missing key would (anti-enumeration).
func (s *Service) configForTenantScoped(ctx context.Context, tenantKey string) (*models.StorageApp, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	app, err := dal.GetAppForTenant(ctx, s.db, tenantKey)
	if err != nil {
		return nil, err
	}
	if app == nil {
		return nil, xcodes.ErrAppNotFound.New(fmt.Sprintf("no tenant config for tenant_key %q", tenantKey))
	}
	if err := authorizeAppTenant(scope, app, xcodes.ErrAppNotFound.New(fmt.Sprintf("no tenant config for tenant_key %q", tenantKey))); err != nil {
		return nil, err
	}
	return app, nil
}

// --- helpers ---

func appToProto(a *models.StorageApp) *storagev1.StorageTenantConfigInfo {
	return &storagev1.StorageTenantConfigInfo{
		Id:        a.ID,
		AppKey:    a.AppKey,
		Name:      a.Name,
		KeyPrefix: a.KeyPrefix,
		BucketId:  a.BucketID,
		Disabled:  a.Disabled,
		CreatedAt: a.CreatedAt.Unix(),
		UpdatedAt: a.UpdatedAt.Unix(),
		TenantKey: models.TenantKeyOf(a.TenantKey),
	}
}

func appSnapshot(a *models.StorageApp) map[string]any {
	return map[string]any{
		"app_key": a.AppKey, "name": a.Name, "key_prefix": a.KeyPrefix,
		"bucket_id": a.BucketID, "disabled": a.Disabled,
		"tenant_key": models.TenantKeyOf(a.TenantKey),
	}
}

// mintUniqueAppKey mints app_keys with collision retry (message-service
// shape): up to 3 attempts against the live registry.
func (s *Service) mintUniqueAppKey(ctx context.Context) (string, error) {
	for range 3 {
		candidate := mintAppKey()
		_, err := dal.GetAppByKey(ctx, s.db, candidate)
		if errors.Is(err, xcodes.ErrAppNotFound.New()) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", xcodes.ErrInternal.New("mint app key: too many collisions")
}

// mintAppKey mints "sto_" + 8 base36 chars (same shape as message-service).
func mintAppKey() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "sto_" + fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = base36[int(b)%36]
	}
	return "sto_" + string(out)
}
