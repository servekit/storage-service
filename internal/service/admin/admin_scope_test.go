// Admin actor-scope matrix (tenant platform phase ④ T5): the three caller
// states the storage admin surface must branch on —
//
//	injected key   → pinned: the app platform answers only the row mapped
//	                 to that tenant (creates clamp to the injection); every
//	                 dimension-less Admin* surface (files/quota/stats/
//	                 providers/buckets/settings/owner/audit) refuses
//	PLATFORM actor → cross-view full surface
//	no identity    → fail closed
//
// Foreign apps answer the domain not-found error (anti-enumeration).
// Fixtures disagree on purpose: the body names beta while the injection
// says alpha.
package admin

import (
	"context"
	"testing"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	userv1 "github.com/servekit/api/gen/go/user/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scopeAlpha = "ten_alpha0000000"
	scopeBeta  = "ten_beta0000000"
)

func platformCtx() context.Context {
	return grpcx.WithActor(context.Background(), &commonv1.RequestActor{
		UserId:   7,
		UserType: int32(userv1.UserType_USER_TYPE_PLATFORM),
	})
}

func tenantCtx(key string) context.Context {
	return tenantctx.WithTenantKey(context.Background(), key)
}

func anonCtx() context.Context { return context.Background() }

func newScopeFixture(t *testing.T) *Service {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	reg, err := storage.NewRegistry(nil)
	require.NoError(t, err)
	gid := &seqGID{}
	return New(&Deps{DB: db, GID: gid, Registry: reg, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder()})
}

// seedScopedApps plants one app row per tenant.
func seedScopedApps(t *testing.T, s *Service) {
	t.Helper()
	ctx := context.Background()
	for i, tc := range []struct{ appKey, prefix, tenant string }{
		{"alpha-app", "alpha/", scopeAlpha},
		{"beta-app", "beta/", scopeBeta},
	} {
		app := &models.StorageApp{
			ID: int64(9000 + i), AppKey: tc.appKey, Name: tc.appKey,
			KeyPrefix: tc.prefix, TenantKey: models.TenantKeyPtr(tc.tenant),
		}
		require.NoError(t, dal.CreateApp(ctx, s.db, app))
	}
}

// TestAdminScope_NoIdentityFailsClosed: neither an injected key nor a
// verified actor → every admin surface refuses.
func TestAdminScope_NoIdentityFailsClosed(t *testing.T) {
	svc := newScopeFixture(t)
	ctx := anonCtx()

	_, err := svc.AdminListTenantConfigs(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = svc.AdminEnsureTenantConfig(ctx, ensureReq("x", "x/", ""))
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = svc.AdminListFiles(ctx, &storagev1.AdminListFilesRequest{})
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = svc.AdminGetQuota(ctx, &storagev1.AdminGetQuotaRequest{})
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())

	_, err = svc.AdminListProviders(ctx, nil)
	require.ErrorIs(t, err, xcodes.ErrUnauthorized.New())
}

// TestAdminScope_AppPlatformBranch: the app platform under an injected key
// — lists answer the own row only; foreign rows answer not-found; creates
// clamp tenant_key to the injection; the cross-view sees and does all.
func TestAdminScope_AppPlatformBranch(t *testing.T) {
	svc := newScopeFixture(t)
	seedScopedApps(t, svc)
	ctx := tenantCtx(scopeAlpha)

	list, err := svc.AdminListTenantConfigs(ctx, nil)
	require.NoError(t, err)
	require.Len(t, list.GetConfigs(), 1)
	assert.Equal(t, "alpha-app", list.GetConfigs()[0].GetAppKey())

	_, err = svc.AdminGetTenantConfig(ctx, &storagev1.AdminGetTenantConfigRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = svc.AdminUpdateTenantConfig(ctx, &storagev1.AdminUpdateTenantConfigRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = svc.AdminDeleteTenantConfig(ctx, &storagev1.AdminDeleteTenantConfigRequest{TenantKey: scopeBeta})
	require.ErrorIs(t, err, xcodes.ErrAppNotFound.New())

	_, err = svc.AdminGetTenantConfig(ctx, &storagev1.AdminGetTenantConfigRequest{TenantKey: scopeAlpha})
	require.NoError(t, err)

	// ensure clamps: body forges beta's tenant, injection wins; a fresh
	// tenant gets its one-row budget
	created, err := svc.AdminEnsureTenantConfig(tenantCtx("ten_gamma0000000"), ensureReq("forged", "forged/", scopeAlpha))
	require.NoError(t, err)
	assert.Equal(t, "ten_gamma0000000", created.GetConfig().GetTenantKey())

	// cross-view full access
	all, err := svc.AdminListTenantConfigs(platformCtx(), nil)
	require.NoError(t, err)
	assert.Len(t, all.GetConfigs(), 3)
}

// TestAdminScope_PlatformOnlySurfaces: the dimension-less Admin* surfaces
// (owner/files/quota/stats/providers/buckets/settings) admit only the
// PLATFORM cross-view — a tenant-scoped caller is refused even though the
// console once showed these pages.
func TestAdminScope_PlatformOnlySurfaces(t *testing.T) {
	svc := newScopeFixture(t)
	ctx := tenantCtx(scopeAlpha)

	_, err := svc.AdminListFiles(ctx, &storagev1.AdminListFilesRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminGetFile(ctx, &storagev1.AdminGetFileRequest{FileId: 1})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminDeleteFile(ctx, &storagev1.AdminDeleteFileRequest{FileId: 1})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminGetQuota(ctx, &storagev1.AdminGetQuotaRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminSetQuota(ctx, &storagev1.AdminSetQuotaRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminGetStats(ctx, &storagev1.AdminGetStatsRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminListProviders(ctx, nil)
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminListBuckets(ctx, nil)
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminCreateProvider(ctx, &storagev1.AdminCreateProviderRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminUpsertBucket(ctx, &storagev1.AdminUpsertBucketRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminSoftDeleteOwnerFiles(ctx, &storagev1.AdminSoftDeleteOwnerFilesRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminDeleteOwner(ctx, &storagev1.AdminDeleteOwnerRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminGetSettings(ctx, &storagev1.AdminGetSettingsRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	_, err = svc.AdminUpdateSettings(ctx, &storagev1.AdminUpdateSettingsRequest{})
	require.ErrorIs(t, err, xcodes.ErrForbidden.New())

	// cross-view passes the guard (surfaces themselves need no data here —
	// empty registries answer empty lists)
	resp, err := svc.AdminListProviders(platformCtx(), nil)
	require.NoError(t, err)
	assert.Empty(t, resp.GetProviders())
}
