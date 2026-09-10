package handler

import (
	"context"
	"testing"
	"time"

	"github.com/servekit/go-common/dbx"

	"github.com/servekit/storage-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrate_Idempotent verifies a second run on an already-migrated DB
// is a no-op.
func TestMigrate_Idempotent(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)

	require.NoError(t, Migrate(db))
	require.NoError(t, Migrate(db),
		"re-running migrate on a clean DB must not error")
}

// TestMigrate_Phase3TenantKeyBackfill: pre-③ rows (tenant_key NULL) are
// backfilled by Migrate — apps map to their app_key literal, files and
// upload_sessions map through their app_key. Rows with an empty app_key
// (pre-app era) keep NULL: no tenant attribution is possible.
func TestMigrate_Phase3TenantKeyBackfill(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, Migrate(db)) // schema exists; no data to backfill yet

	seedCtx := context.Background()
	require.NoError(t, db.WithContext(seedCtx).Create(&models.StorageApp{
		ID: 1, AppKey: "testkit", AppSecret: "s", Name: "testkit", KeyPrefix: "testkit/",
	}).Error)
	require.NoError(t, db.WithContext(seedCtx).Create(&models.StorageFile{
		ID: 11, OwnerType: 1, OwnerID: 1, ObjectID: 1, AppKey: "testkit", Filename: "mapped.bin",
	}).Error)
	require.NoError(t, db.WithContext(seedCtx).Create(&models.StorageFile{
		ID: 12, OwnerType: 1, OwnerID: 2, ObjectID: 1, AppKey: "", Filename: "pre-app.bin",
	}).Error)
	require.NoError(t, db.WithContext(seedCtx).Create(&models.StorageUploadSession{
		ID: 21, OwnerType: 1, OwnerID: 1, Bucket: "uploads", ObjectKey: "testkit/x",
		AppKey: "testkit", KeyPrefix: "testkit/", MD5: "m", Size: 1,
		ContentType: "text/plain", Filename: "s.bin", Vendor: 1,
		Status: 0, ExpiresAt: time.Now().Add(time.Hour),
	}).Error)

	require.NoError(t, Migrate(db), "backfill run must succeed")

	var app models.StorageApp
	require.NoError(t, db.Where("id = ?", 1).First(&app).Error)
	assert.Equal(t, "testkit", models.TenantKeyOf(app.TenantKey), "app maps to its app_key literal")

	var mapped, preApp models.StorageFile
	require.NoError(t, db.Where("id = ?", 11).First(&mapped).Error)
	assert.Equal(t, "testkit", models.TenantKeyOf(mapped.TenantKey), "file backfilled through the app mapping")
	require.NoError(t, db.Where("id = ?", 12).First(&preApp).Error)
	assert.Nil(t, preApp.TenantKey, "empty-app_key (pre-app era) rows keep NULL — no attribution possible")

	var sess models.StorageUploadSession
	require.NoError(t, db.Where("id = ?", 21).First(&sess).Error)
	assert.Equal(t, "testkit", models.TenantKeyOf(sess.TenantKey))

	// Third run: fully backfilled → reconcile passes, still idempotent.
	require.NoError(t, Migrate(db))
}

// TestMigrate_Phase3ReconcileAborts: a non-empty app_key that matches no
// app row (hard-deleted app) can never be backfilled — reconcile must abort
// the migration loudly instead of silently leaving the row unmapped.
func TestMigrate_Phase3ReconcileAborts(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, Migrate(db))

	require.NoError(t, db.WithContext(context.Background()).Create(&models.StorageUploadSession{
		ID: 31, OwnerType: 1, OwnerID: 1, Bucket: "uploads", ObjectKey: "ghost/x",
		AppKey: "ghost-app", KeyPrefix: "ghost/", MD5: "m", Size: 1,
		ContentType: "text/plain", Filename: "g.bin", Vendor: 1,
		Status: 0, ExpiresAt: time.Now().Add(time.Hour),
	}).Error)

	err := Migrate(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reconcile", "the failure must name the reconcile step")
}
