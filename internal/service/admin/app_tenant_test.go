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

// TestAdminCreateApp_TenantKey: the admin surface accepts and echoes the
// phase ③ tenant mapping; an omitted tenant_key defaults to the app_key
// literal (same mapping the migration backfill writes); a duplicate
// tenant_key is rejected before hitting the DB unique index.
func TestAdminCreateApp_TenantKey(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	reg, err := storage.NewRegistry(nil)
	require.NoError(t, err)
	gid := &seqGID{}
	svc := New(&Deps{DB: db, GID: gid, Registry: reg, Audit: audit.New(&audit.Deps{DB: db, GID: gid}).Recorder()})
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	resp, err := svc.AdminCreateApp(ctx, adminCreateAppReq("mapped", "mapped/", "ten_acme00000001"))
	require.NoError(t, err)
	assert.Equal(t, "ten_acme00000001", resp.GetApp().GetTenantKey())

	row, err := dal.GetAppByKey(ctx, db, "mapped")
	require.NoError(t, err)
	assert.Equal(t, "ten_acme00000001", models.TenantKeyOf(row.TenantKey))

	// Omitted tenant_key defaults to the app_key literal (window mapping).
	resp, err = svc.AdminCreateApp(ctx, adminCreateAppReq("literal", "literal/", ""))
	require.NoError(t, err)
	assert.Equal(t, "literal", resp.GetApp().GetTenantKey())

	// Duplicate tenant_key fails with a friendly error, not an internal one.
	_, err = svc.AdminCreateApp(ctx, adminCreateAppReq("clash", "clash/", "ten_acme00000001"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "INTERNAL", "duplicate tenant_key must not surface as an internal error")

	// AdminListApps echoes the mapping.
	list, err := svc.AdminListApps(ctx, nil)
	require.NoError(t, err)
	byKey := map[string]string{}
	for _, a := range list.GetApps() {
		byKey[a.GetAppKey()] = a.GetTenantKey()
	}
	assert.Equal(t, "ten_acme00000001", byKey["mapped"])
	assert.Equal(t, "literal", byKey["literal"])
}

func adminCreateAppReq(appKey, keyPrefix, tenantKey string) *storagev1.AdminCreateAppRequest {
	return &storagev1.AdminCreateAppRequest{
		AppKey: appKey, Name: appKey, KeyPrefix: keyPrefix, TenantKey: tenantKey,
	}
}
