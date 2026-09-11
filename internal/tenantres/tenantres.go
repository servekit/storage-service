// Package tenantres resolves the upload path's calling identity. Since the
// ④ window close the trusted x-tenant-key (injected by the portal proxy)
// is the ONLY credential stack:
//
//   - the key is format-validated first (tenantctx.ValidTenantKey — the
//     canonical ten_[0-9a-z]{12} shape plus the reserved ten_platform /
//     ten_legacy literals); malformed keys fail closed before any lookup
//     or lazy create;
//   - the per-tenant config row (storage_apps row carrying the key_prefix
//     namespace) is resolved by the tenant_key mapping and lazily upserted
//     on first sight with the derived default key_prefix "{tenant_key}/" —
//     EXISTING rows keep their stored prefix verbatim (immutability:
//     objects already live under it, and it is the dedup domain).
//
// Verification is service-layer (not an interceptor) so module-mode
// in-process callers share the same path as gRPC clients.
package tenantres

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/servekit/go-common/tenantctx"

	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/internal/store/dal"
	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/xcodes"
)

// Caller is the resolved upload-path identity: the tenant context downstream
// keying hangs off (session/file rows, token namespace) plus the config row
// that owns the caller's key_prefix namespace and bucket binding.
type Caller struct {
	// TenantKey is the authoritative tenant context ("ten_..." or one of
	// the reserved literals).
	TenantKey string
	// App is the tenant's (possibly just-created) config row.
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

// Require resolves the Caller from the trusted x-tenant-key, failing closed
// with ErrAppUnauthorized when the credential is missing or malformed, or
// the resolved config row is unknown or disabled. Call at the entry of
// every credential-presenting surface (GenerateUploadURL / ConfirmUpload /
// GetSTSCredential / BatchGetSTSCredential / CancelUpload).
func (r *Resolver) Require(ctx context.Context) (*Caller, error) {
	tenantKey, ok := tenantctx.TrustedKeyFromIncoming(ctx)
	if !ok {
		return nil, xcodes.ErrAppUnauthorized.New(
			"missing trusted caller credential (x-tenant-key metadata)")
	}
	if !tenantctx.ValidTenantKey(tenantKey) {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("malformed x-tenant-key %q", tenantKey))
	}
	return r.ensureTrusted(ctx, tenantKey)
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
		if err := dal.EnsureTenantApp(ctx, r.db, &models.StorageApp{
			AppKey:    tenantKey,
			TenantKey: models.TenantKeyPtr(tenantKey),
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

// callerFromRow fails closed on a disabled config row.
func callerFromRow(tenantKey string, app *models.StorageApp) (*Caller, error) {
	if app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("app %q not found or disabled", tenantKey))
	}
	return &Caller{TenantKey: tenantKey, App: app}, nil
}

// --- internal helpers ---
