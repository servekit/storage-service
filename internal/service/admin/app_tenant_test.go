package admin

import (
	"context"
	"sync/atomic"
	"testing"

	gidv1 "github.com/servekit/api/gen/go/gid/v1"
	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/audit"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seqGID is a gidservice.Service returning sequential IDs with no external
// dependency (same shape as the upload package's test fake).
type seqGID struct {
	gidv1.UnimplementedGidServiceServer
	counter int64
}

func (g *seqGID) NextID(_ context.Context, _ *gidv1.NextIDRequest) (*gidv1.NextIDResponse, error) {
	return &gidv1.NextIDResponse{Id: atomic.AddInt64(&g.counter, 1)}, nil
}

// TestAdminEnsureTenantConfig_TenantKey (phase ④ T6 rename of
// TestAdminCreateApp_TenantKey): the surface accepts and echoes the tenant
// mapping; an omitted tenant_key on the cross-view defaults to the
// server-minted app key literal (same mapping the migration backfill
// writes); re-ensuring an already-mapped tenant is IDEMPOTENT — the live
// row (and its secret) is returned as-is instead of a duplicate-mapping
// error, the semantics the new name promises.
func TestAdminEnsureTenantConfig_TenantKey(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	reg, err := storage.NewRegistry(nil)
	require.NoError(t, err)
	gid := &seqGID{}
	svc := New(&Deps{DB: db, GID: gid, Registry: reg, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder()})
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	resp, err := svc.AdminEnsureTenantConfig(ctx, ensureReq("mapped", "mapped/", "ten_acme00000001"))
	require.NoError(t, err)
	assert.Equal(t, "ten_acme00000001", resp.GetConfig().GetTenantKey())

	row, err := dal.GetAppForTenant(ctx, db, "ten_acme00000001")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "ten_acme00000001", models.TenantKeyOf(row.TenantKey))
	assert.NotEmpty(t, row.AppKey, "the app identity is server-minted now")

	// Idempotent: re-ensuring the mapped tenant returns the SAME row.
	again, err := svc.AdminEnsureTenantConfig(ctx, ensureReq("other-name", "other/", "ten_acme00000001"))
	require.NoError(t, err)
	assert.Equal(t, row.ID, again.GetConfig().GetId(), "ensure of an existing tenant is a read-back, not a create")
	assert.Equal(t, row.AppKey, again.GetConfig().GetAppKey())

	// Omitted tenant_key defaults to the minted app_key literal (window
	// mapping) — read back through the row since the wire never names it.
	resp, err = svc.AdminEnsureTenantConfig(ctx, ensureReq("literal", "literal/", ""))
	require.NoError(t, err)
	literal, err := dal.GetAppForTenant(ctx, db, resp.GetConfig().GetAppKey())
	require.NoError(t, err)
	require.NotNil(t, literal)
	assert.Equal(t, literal.AppKey, models.TenantKeyOf(literal.TenantKey))

	// AdminListTenantConfigs echoes the mapping.
	list, err := svc.AdminListTenantConfigs(ctx, nil)
	require.NoError(t, err)
	byKey := map[string]string{}
	for _, a := range list.GetConfigs() {
		byKey[a.GetAppKey()] = a.GetTenantKey()
	}
	assert.Equal(t, "ten_acme00000001", byKey[row.AppKey])
	assert.Equal(t, literal.AppKey, byKey[literal.AppKey])
}

func ensureReq(name, keyPrefix, tenantKey string) *storagev1.AdminEnsureTenantConfigRequest {
	return &storagev1.AdminEnsureTenantConfigRequest{
		Name: name, KeyPrefix: keyPrefix, TenantKey: tenantKey,
	}
}
