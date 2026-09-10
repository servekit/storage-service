package tenantres

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/service/platform"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/tenantctx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// legacyCtx plants the deleted stack's wire shape (a complete x-app-key/
// x-app-secret pair) — anti-regression fixtures only.
func legacyCtx(ctx context.Context, appKey, appSecret string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(
		"x-app-key", appKey,
		"x-app-secret", appSecret,
	))
}

func setup(t *testing.T) (*Resolver, *gorm.DB, *storage.Registry) {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	reg, err := storage.NewRegistry(nil)
	require.NoError(t, err)
	return New(db, reg), db, reg
}

// seedApp inserts an app row directly (raw column control) and converges the
// registry snapshot (production path: platform.LoadAndRebuild) so both the
// legacy and tenant indexes see it.
func seedApp(t *testing.T, db *gorm.DB, reg *storage.Registry, id int64, appKey, secret, keyPrefix string, tenantKey *string) {
	t.Helper()
	app := &models.StorageApp{
		ID: id, AppKey: appKey, AppSecret: secret, Name: appKey,
		KeyPrefix: keyPrefix, TenantKey: tenantKey,
	}
	require.NoError(t, dal.CreateApp(context.Background(), db, app))
	require.NoError(t, platform.LoadAndRebuild(context.Background(), db, reg, "", ""))
}

// TestRequireTrustedLazilyCreatesConfigRowWithDerivedPrefix: a first-sight
// trusted tenant gets a default config row whose key_prefix is derived as
// "{tenant_key}/" (spec: new tenants only — existing prefixes are never
// recomputed), a minted secret nobody holds, and the default bucket binding;
// the second call reuses the row and resolves through the registry snapshot.
func TestRequireTrustedLazilyCreatesConfigRowWithDerivedPrefix(t *testing.T) {
	r, db, reg := setup(t)
	ctx := tenantctx.WithTenant(context.Background(), "ten_abc123def456")

	c1, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_abc123def456", c1.TenantKey)
	require.NotNil(t, c1.App)
	assert.Equal(t, "ten_abc123def456", c1.App.AppKey)
	assert.Equal(t, "ten_abc123def456", models.TenantKeyOf(c1.App.TenantKey))
	assert.Equal(t, "ten_abc123def456/", c1.App.KeyPrefix, "new tenant derives key_prefix = {tenant_key}/")
	assert.NotEmpty(t, c1.App.AppSecret, "minted secret satisfies the not-null column")
	assert.NotEqual(t, "ten_abc123def456", c1.App.AppSecret, "the minted secret must not be guessable")
	assert.Zero(t, c1.App.BucketID, "lazy config row binds the default bucket")
	assert.False(t, c1.App.Disabled)

	c2, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, c1.App.ID, c2.App.ID, "second call must reuse the config row")
	assert.NotNil(t, reg.AppByTenant("ten_abc123def456"), "registry snapshot converged after first sight")

	apps, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, apps, 1)
}

// TestRequireTrustedKeepsExistingPrefixImmutable pins the storage-specific
// invariant: an existing tenant's key_prefix is NEVER recomputed to
// "{tenant_key}/" — objects already live under the stored prefix, which is
// also the dedup domain.
func TestRequireTrustedKeepsExistingPrefixImmutable(t *testing.T) {
	r, db, _ := setup(t)
	const key = "ten_keep00000000"
	seedApp(t, db, r.reg, 10, "sto_8blto9j5", "s0", "demo-b/", models.TenantKeyPtr(key))

	c, err := r.Require(tenantctx.WithTenant(context.Background(), key))
	require.NoError(t, err)
	require.NotNil(t, c.App)
	assert.Equal(t, int64(10), c.App.ID, "the existing config row must be reused")
	assert.Equal(t, "demo-b/", c.App.KeyPrefix, "existing prefixes are immutable — never recomputed")

	apps, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, apps, 1, "no duplicate row for an existing tenant")
}

// TestRequireTrustedAuthoritativeOverSmuggledLegacyCreds pins D-③1: when
// x-tenant-key rides together with a legacy ak/sk pair (of a DIFFERENT
// tenant's app, valid secret), the trusted key wins and the legacy pair is
// discarded — a proxy-forwarded caller cannot impersonate another tenant.
func TestRequireTrustedAuthoritativeOverSmuggledLegacyCreds(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 20, "beta-app", "beta-secret", "beta/", models.TenantKeyPtr("ten_beta00000000"))

	ctx := tenantctx.WithTenant(legacyCtx(context.Background(), "beta-app", "beta-secret"), "ten_alpha0000000")
	c, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_alpha0000000", c.TenantKey)
	assert.NotEqual(t, int64(20), c.App.ID, "the smuggled app row must not be the caller")
}

// TestRequireLegacyCredentialsRejected (④ window close): the legacy ak/sk
// stack no longer authenticates — even previously-VALID pairs (mapped or
// unmapped) answer ErrAppUnauthorized. Pinned against accidental
// resurrection of the deleted path.
func TestRequireLegacyCredentialsRejected(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 30, "legacy-mapped", "s1", "mapped/", models.TenantKeyPtr("ten_mapped000000"))
	seedApp(t, db, reg, 31, "legacy-unmapped", "s2", "unmapped/", nil)

	_, err := r.Require(legacyCtx(context.Background(), "legacy-mapped", "s1"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a previously-valid mapped pair must now be unauthenticated")

	_, err = r.Require(legacyCtx(context.Background(), "legacy-unmapped", "s2"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a previously-valid unmapped pair must now be unauthenticated")

	_, err = r.Require(legacyCtx(context.Background(), "legacy-mapped", "wrong"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())
}

// TestRequireTrustedMalformedKeyRejected (④ window close): the trusted key
// is format-validated before any registry/DB lookup or lazy create —
// legacy app_key literals and other malformed values answer
// ErrAppUnauthorized and never mint a config row.
func TestRequireTrustedMalformedKeyRejected(t *testing.T) {
	r, db, _ := setup(t)

	for _, key := range []string{
		"beta-app",            // legacy app_key literal
		"sto_8blto9j5",        // minted app_key shape
		"testkit",             // legacy alias literal
		"ten_UPPERCASE00",     // uppercase
		"ten_short0",          // too short
		"ten_abc123def456789", // too long
		"ten_platform_system", // reserved literal with a suffix
	} {
		_, err := r.Require(tenantctx.WithTenant(context.Background(), key))
		assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "malformed key %q must fail closed", key)
	}
	apps, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, apps, "malformed keys must not lazily create config rows")
}

// TestRequireTrustedReusesUnbackfilledRow: a trusted key equal to an existing
// app_key whose tenant_key column is still NULL (SQL not yet run) must reuse
// that row instead of creating a duplicate — and must keep its prefix.
func TestRequireTrustedReusesUnbackfilledRow(t *testing.T) {
	r, db, _ := setup(t)
	const key = "ten_backfill0000"
	seedApp(t, db, r.reg, 40, key, "s3", key+"/", nil)

	c, err := r.Require(tenantctx.WithTenant(context.Background(), key))
	require.NoError(t, err)
	assert.Equal(t, int64(40), c.App.ID)
	assert.Equal(t, key+"/", c.App.KeyPrefix, "un-backfilled row keeps its prefix")

	apps, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, apps, 1, "no duplicate row for an app_key-equal trusted key")
}

// TestRequireTrustedRevivesSoftDeletedOccupant pins the F2 hardening: a
// soft-deleted config row occupying the tenant's unique keys made the lazy
// insert silently no-op and the scoped re-read miss — surfacing as the
// one-off INTERNAL "row absent after insert" (T11 F2 anomaly, the T6-recorded
// soft-delete-occupant edge). Ensure semantics now revive the occupant in
// place: un-delete, re-point tenant_key, keep the historic identity fields
// (key_prefix immutable — objects may live under it; app_secret; name).
func TestRequireTrustedRevivesSoftDeletedOccupant(t *testing.T) {
	r, db, _ := setup(t)
	const tenantKey = "ten_dead00000000"

	// Occupant shape 1: a lazily-shaped row (app_key = tenant_key, mapping
	// column set) that an operator soft-deleted via the admin surface.
	dead := &models.StorageApp{
		AppKey: tenantKey, AppSecret: "old-secret", Name: "old name",
		KeyPrefix: "ten_dead0000000/", TenantKey: models.TenantKeyPtr(tenantKey),
	}
	require.NoError(t, db.Create(dead).Error)
	require.NoError(t, db.Model(&models.StorageApp{}).Where("id = ?", dead.ID).
		Update("deleted_at", time.Now()).Error)

	c, err := r.Require(tenantctx.WithTenant(context.Background(), tenantKey))
	require.NoError(t, err, "a soft-deleted occupant must be revived, not 500")
	require.Equal(t, tenantKey, c.TenantKey)
	require.Equal(t, dead.ID, c.App.ID, "the occupant row is revived in place")
	require.Equal(t, "ten_dead0000000/", c.App.KeyPrefix, "historic prefix kept verbatim (immutability invariant)")
	require.Equal(t, "old-secret", c.App.AppSecret, "historic secret kept (revive ≠ re-mint)")

	var deletedCount int64
	require.NoError(t, db.Unscoped().Model(&models.StorageApp{}).
		Where("id = ? AND deleted_at IS NOT NULL", dead.ID).Count(&deletedCount).Error)
	require.Zero(t, deletedCount, "row must be live again")

	live, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, live, 1, "exactly one row for the tenant")
}

// TestRequireTrustedRevivesSoftDeletedUnmappedOccupant: the same edge via
// the app_key-equal fallback shape — a soft-deleted pre-backfill row (NULL
// tenant_key) occupying the app_key unique index. Revive must ALSO re-point
// the mapping column so subsequent resolution prefers tenant_key.
func TestRequireTrustedRevivesSoftDeletedUnmappedOccupant(t *testing.T) {
	r, db, _ := setup(t)
	const tenantKey = "ten_unmapped0000"

	dead := &models.StorageApp{
		AppKey: tenantKey, AppSecret: "s", Name: tenantKey,
		KeyPrefix: tenantKey + "/", TenantKey: nil,
	}
	require.NoError(t, db.Create(dead).Error)
	require.NoError(t, db.Model(&models.StorageApp{}).Where("id = ?", dead.ID).
		Update("deleted_at", time.Now()).Error)

	c, err := r.Require(tenantctx.WithTenant(context.Background(), tenantKey))
	require.NoError(t, err)
	require.Equal(t, dead.ID, c.App.ID)
	require.Equal(t, tenantKey, models.TenantKeyOf(c.App.TenantKey),
		"revive re-points the mapping column")
}

// TestRequireTrustedLiveOccupantPreferredOverDead (③-review F2 corner): when
// the unscoped occupant lookup can match BOTH a LIVE row and a soft-deleted
// one (a pre-backfill live row on app_key=tenant_key with a NULL mapping
// column, plus a soft-deleted alias row still holding tenant_key), the
// live-first ordering must pick the live row — the dead one stays dead
// instead of being revived into a second claimant of the tenant's keys.
func TestRequireTrustedLiveOccupantPreferredOverDead(t *testing.T) {
	r, db, _ := setup(t)
	const tenantKey = "ten_live00000000"

	live := &models.StorageApp{
		AppKey: tenantKey, AppSecret: "live-secret", Name: "live",
		KeyPrefix: tenantKey + "/", TenantKey: nil,
	}
	require.NoError(t, db.Create(live).Error)
	dead := &models.StorageApp{
		AppKey: "sto_oldalias2", AppSecret: "old-secret", Name: "dead",
		KeyPrefix: "old/", TenantKey: models.TenantKeyPtr(tenantKey),
	}
	require.NoError(t, db.Create(dead).Error)
	require.NoError(t, db.Model(&models.StorageApp{}).Where("id = ?", dead.ID).
		Update("deleted_at", time.Now()).Error)

	c, err := r.Require(tenantctx.WithTenant(context.Background(), tenantKey))
	require.NoError(t, err)
	require.Equal(t, live.ID, c.App.ID, "the LIVE occupant must win the unscoped lookup")
	require.Equal(t, "live-secret", c.App.AppSecret)

	var still int64
	require.NoError(t, db.Unscoped().Model(&models.StorageApp{}).
		Where("id = ? AND deleted_at IS NOT NULL", dead.ID).Count(&still).Error)
	require.EqualValues(t, 1, still, "the dead occupant must stay dead when a live one exists")
}

// TestRequireFailureModes: no credentials / a previously-valid legacy pair /
// a disabled config row all fail closed with ErrAppUnauthorized.
func TestRequireFailureModes(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 50, "known", "right", "known/", nil)

	_, err := r.Require(legacyCtx(context.Background(), "known", "right"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a previously-valid legacy pair must now be unauthenticated")

	_, err = r.Require(legacyCtx(context.Background(), "ghost", "whatever"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(context.Background())
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	ctx := tenantctx.WithTenant(context.Background(), "ten_dead00000000")
	c, err := r.Require(ctx)
	require.NoError(t, err)
	c.App.Disabled = true
	require.NoError(t, dal.UpdateApp(context.Background(), db, c.App))
	require.NoError(t, platform.LoadAndRebuild(context.Background(), db, reg, "", ""))
	_, err = r.Require(ctx)
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a disabled config row fails closed")
}
