package upload

import (
	"context"
	"testing"

	storagev1 "github.com/servekit/api/gen/go/storage/v1"
	"github.com/servekit/storage-service/internal/appauth"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase ③ dual-stack data-plane coverage (recipe step 1 for storage): the
// trusted x-tenant-key path lazily derives "{tenant_key}/" prefixes for
// first-sight tenants, existing prefixes are never recomputed, smuggled
// legacy credentials under a trusted key are ignored, and the legacy ak/sk
// path keeps working with tenant conversion on the write path.

// tenantCtx wraps ctx with the trusted tenant key.
func tenantCtx(ctx context.Context, tenantKey string) context.Context {
	return appauth.WithTenant(ctx, tenantKey)
}

// TestGenerateUploadURL_TrustedNewTenantDerivesPrefix: a first-sight trusted
// tenant uploads without any pre-registered app — the config row is lazily
// created with key_prefix "{tenant_key}/" and every issued key lives under
// it. The session row snapshots the tenant.
func TestGenerateUploadURL_TrustedNewTenantDerivesPrefix(t *testing.T) {
	svc, _, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	const tenant = "ten_news00000001"

	resp, err := svc.GenerateUploadURL(tenantCtx(context.Background(), tenant), &storagev1.GenerateUploadURLRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 400}, Md5: "c0000000000000000000000000000001",
		Size: 4, Filename: "new-tenant.bin",
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant(), "fresh md5 must not instant-upload")
	assert.Contains(t, resp.GetObjectKey(), tenant+"/",
		"issued staging key must live under the derived {tenant_key}/ prefix")

	var sess models.StorageUploadSession
	require.NoError(t, db.Where("owner_id = ?", 400).First(&sess).Error)
	assert.Equal(t, tenant, sess.AppKey)
	assert.Equal(t, tenant+"/", sess.KeyPrefix, "session snapshots the derived prefix")
	assert.Equal(t, tenant, models.TenantKeyOf(sess.TenantKey), "session snapshots the tenant (write-path switch)")

	var app models.StorageApp
	require.NoError(t, db.Where("app_key = ?", tenant).First(&app).Error,
		"first-sight trusted tenant must lazily create the config row")
	assert.Equal(t, tenant+"/", app.KeyPrefix)
	assert.Equal(t, tenant, models.TenantKeyOf(app.TenantKey))
	assert.NotEmpty(t, app.AppSecret, "minted secret satisfies the not-null column")
}

// TestConfirmUpload_TrustedFlowWritesTenantKey drives the full trusted flow:
// issue → PUT staging bytes → confirm. The confirmed file row carries the
// tenant and the object lands under the derived prefix.
func TestConfirmUpload_TrustedFlowWritesTenantKey(t *testing.T) {
	svc, fp, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	const tenant = "ten_full00000002"
	const md5 = "c0000000000000000000000000000002"
	ctx := tenantCtx(context.Background(), tenant)

	resp, err := svc.GenerateUploadURL(ctx, &storagev1.GenerateUploadURLRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 401}, Md5: md5,
		Size: 4, Filename: "trusted.bin",
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant())
	fp.PutObjectWithMD5(context.Background(), "uploads", resp.GetObjectKey(), []byte("data"), "text/plain", md5)

	confirmed, err := svc.ConfirmUpload(ctx, &storagev1.ConfirmUploadRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 401}, UploadToken: resp.GetUploadToken(),
	})
	require.NoError(t, err)
	require.NotZero(t, confirmed.GetFileId())

	file, err := dal.GetFileByID(context.Background(), db, confirmed.GetFileId())
	require.NoError(t, err)
	assert.Equal(t, tenant, models.TenantKeyOf(file.TenantKey), "confirmed file carries the tenant")
	assert.Equal(t, tenant, file.AppKey)

	obj, err := dal.GetObjectByID(context.Background(), db, file.ObjectID)
	require.NoError(t, err)
	assert.Contains(t, obj.ObjectKey, tenant+"/", "object lands under the derived prefix")
}

// TestGenerateUploadURL_LegacyKeepsPrefixAndCarriesTenant: the legacy ak/sk
// path is unchanged — the existing "uploads/" prefix is used verbatim (never
// recomputed) and the session carries the mapped tenant (app_key literal
// fallback while the column is NULL).
func TestGenerateUploadURL_LegacyKeepsPrefixAndCarriesTenant(t *testing.T) {
	svc, _, db := setupUploadServiceWithFakeProvider(t, noopHost{})

	resp, err := svc.GenerateUploadURL(appCtx(context.Background()), &storagev1.GenerateUploadURLRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 402}, Md5: "c0000000000000000000000000000003",
		Size: 4, Filename: "legacy.bin",
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant())
	assert.Contains(t, resp.GetObjectKey(), "uploads/", "legacy app keeps its stored prefix")

	var sess models.StorageUploadSession
	require.NoError(t, db.Where("owner_id = ?", 402).First(&sess).Error)
	assert.Equal(t, "test-app", sess.AppKey)
	assert.Equal(t, "uploads/", sess.KeyPrefix)
	assert.Equal(t, "test-app", models.TenantKeyOf(sess.TenantKey),
		"legacy caller converts to the mapped tenant (app_key literal fallback)")
}

// TestGenerateUploadURL_TrustedAuthoritativeOverSmuggledLegacyCreds pins
// D-③1 on the data plane: valid legacy credentials of a DIFFERENT app riding
// under a trusted tenant key are discarded — the upload proceeds as the
// trusted tenant, never under the smuggled app's prefix.
func TestGenerateUploadURL_TrustedAuthoritativeOverSmuggledLegacyCreds(t *testing.T) {
	svc, _, db := setupUploadServiceWithFakeProvider(t, noopHost{})
	const tenant = "ten_other0000004"

	// appCtx plants test-app/test-secret (valid); WithTenant then adds the
	// trusted key on top — the trusted key must win.
	ctx := tenantCtx(appCtx(context.Background()), tenant)
	resp, err := svc.GenerateUploadURL(ctx, &storagev1.GenerateUploadURLRequest{
		Owner: &storagev1.Owner{OwnerType: 1, OwnerId: 403}, Md5: "c0000000000000000000000000000004",
		Size: 4, Filename: "smuggled.bin",
	})
	require.NoError(t, err)
	require.False(t, resp.GetInstant())
	assert.NotContains(t, resp.GetObjectKey(), "uploads/",
		"the smuggled test-app prefix must NOT be used")
	assert.Contains(t, resp.GetObjectKey(), tenant+"/")

	var sess models.StorageUploadSession
	require.NoError(t, db.Where("owner_id = ?", 403).First(&sess).Error)
	assert.Equal(t, tenant, sess.AppKey)
	assert.Equal(t, tenant+"/", sess.KeyPrefix)

	var apps []models.StorageApp
	require.NoError(t, db.Find(&apps).Error)
	require.Len(t, apps, 1)
	assert.Equal(t, tenant, apps[0].AppKey,
		"only the trusted tenant's config row exists — the smuggled test-app row was never consulted")
}
