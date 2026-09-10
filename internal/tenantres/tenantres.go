// Package tenantres resolves the upload path's calling identity during the
// phase ③ dual-stack window (recipe step 1, rule D-③1):
//
//   - SourceTrusted (x-tenant-key, injected by the portal proxy): the tenant
//     key IS the tenant context. The per-tenant config row (storage_apps row
//     carrying the key_prefix namespace) is resolved by the tenant_key
//     mapping and lazily upserted on first sight with the derived default
//     key_prefix "{tenant_key}/" — EXISTING rows keep their stored prefix
//     verbatim (immutability: objects already live under it, and it is the
//     dedup domain).
//   - SourceLegacy (x-app-key/x-app-secret): the pre-③ app validation,
//     unchanged; the validated app is then converted to the tenant its row
//     maps to (tenant_key column; empty column falls back to the app_key
//     literal — T10 总装 clears the empties).
//   - SourceNone: unauthenticated, fail closed.
//
// The whole package (and the legacy half of appauth) is deleted when the
// window closes (phase ④).
package tenantres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"gorm.io/gorm"

	"github.com/servekit/go-common/dualauth"

	"github.com/servekit/storage-service/internal/appauth"
	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"
)

// Caller is the resolved upload-path identity: the tenant context downstream
// keying hangs off (session/file rows, token namespace) plus the config row
// that owns the caller's key_prefix namespace and bucket binding.
type Caller struct {
	// TenantKey is the authoritative tenant context ("ten_..." or a legacy
	// app_key literal through the window).
	TenantKey string
	// App is the config row: the validated app on the legacy path, the
	// tenant's (possibly just-created) row on the trusted path.
	App *models.StorageApp
}

// Resolver resolves callers. Reads go through the registry snapshot; only
// the first-sight trusted path touches the DB (and converges the snapshot so
// subsequent calls stay lock-free).
type Resolver struct {
	db  *gorm.DB
	reg *storage.Registry
}

// New constructs a Resolver.
func New(db *gorm.DB, reg *storage.Registry) *Resolver {
	return &Resolver{db: db, reg: reg}
}

// Require classifies the caller's credential stack (appauth.Resolve, D-③1)
// and resolves the Caller, failing closed with ErrAppUnauthorized on missing
// credentials, unknown/disabled apps, or a bad secret. Call at the entry of
// every credential-presenting surface (GenerateUploadURL / ConfirmUpload /
// GetSTSCredential / BatchGetSTSCredential / CancelUpload).
func (r *Resolver) Require(ctx context.Context) (*Caller, error) {
	tenantKey, appKey, appSecret, source := appauth.Resolve(ctx)
	switch source {
	case dualauth.SourceTrusted:
		return r.ensureTrusted(ctx, tenantKey)
	case dualauth.SourceLegacy:
		return r.verifyLegacy(appKey, appSecret)
	default:
		return nil, xcodes.ErrAppUnauthorized.New(
			"missing caller credentials (x-tenant-key or x-app-key / x-app-secret metadata)")
	}
}

// ensureTrusted resolves the tenant's config row through the snapshot; on a
// miss it falls back to the DB (covers rows written by other nodes and
// un-backfilled app_key-equal rows) and finally lazily creates the default
// config row with the derived key_prefix "{tenant_key}/". The minted secret
// satisfies the not-null column without being handed to anyone — trusted
// callers authenticate by network position, never by secret. An existing
// row's key_prefix is NEVER recomputed (immutability invariant).
func (r *Resolver) ensureTrusted(ctx context.Context, tenantKey string) (*Caller, error) {
	if app := r.reg.AppByTenant(tenantKey); app != nil {
		return callerFromRow(tenantKey, app)
	}

	app, err := dal.GetAppForTenant(ctx, r.db, tenantKey)
	if err != nil {
		return nil, err
	}
	if app == nil {
		secret, mintErr := mintTenantSecret()
		if mintErr != nil {
			return nil, xcodes.ErrInternal.Wrap(mintErr)
		}
		if err := dal.EnsureTenantApp(ctx, r.db, &models.StorageApp{
			AppKey:    tenantKey,
			TenantKey: models.TenantKeyPtr(tenantKey),
			AppSecret: secret,
			Name:      tenantKey,
			// Spec §storage: new tenants derive "{tenant_key}/"; existing
			// tenants keep their stored prefix (never recomputed).
			KeyPrefix: tenantKey + "/",
		}); err != nil {
			return nil, err
		}
		app, err = dal.GetAppForTenant(ctx, r.db, tenantKey)
		if err != nil {
			return nil, err
		}
		if app == nil {
			return nil, xcodes.ErrInternal.New("ensure tenant config row: row absent after insert")
		}
		// Converge the snapshot so subsequent calls resolve lock-free. Only
		// the app snapshot is merged (provider clients are untouched —
		// rebuilding them mid-request would be wasteful and would discard
		// live state); the platform cron converges the full snapshot within
		// a minute regardless.
		r.reg.MergeApp(app)
	}
	return callerFromRow(tenantKey, app)
}

// verifyLegacy is the pre-③ appauth path, kept verbatim through the
// dual-stack window: unknown/disabled app or a bad secret fails closed (the
// registry snapshot is the read path, same as pre-③ — the cron converges
// cross-node writes). The validated app is then converted to its mapped
// tenant_key (empty column → app_key literal fallback; T10 总装 clears the
// empties).
func (r *Resolver) verifyLegacy(appKey, appSecret string) (*Caller, error) {
	app := r.reg.App(appKey)
	if app == nil || app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("app %q not found or disabled", appKey))
	}
	if app.AppSecret != appSecret {
		return nil, xcodes.ErrAppUnauthorized.New("bad app credentials")
	}
	return &Caller{TenantKey: models.AppTenantKey(app), App: app}, nil
}

// callerFromRow fails closed on a disabled config row.
func callerFromRow(tenantKey string, app *models.StorageApp) (*Caller, error) {
	if app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("app %q not found or disabled", tenantKey))
	}
	return &Caller{TenantKey: tenantKey, App: app}, nil
}

// --- internal helpers ---

// mintTenantSecret mints "sto_" + 32 random bytes (base64url) — same shape
// as admin-minted app secrets; never handed to anyone (trusted callers
// authenticate by network position).
func mintTenantSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint tenant secret: %w", err)
	}
	return "sto_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
