package handler

import (
	"fmt"
	"log/slog"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	"github.com/servekit/storage-service/internal/store/models"
)

// tableName resolves the physical table name for a logical (unprefixed)
// table on db, prefix-aware: convention-named tables carry the naming
// strategy's TablePrefix exactly as AutoMigrate creates them (storage's
// models have no custom TableName() overrides). The post-migrate raw SQL
// must go through this so a prefixed database (dbx TablePrefix) converges
// identically to a bare one — the bare-name pattern this replaces had
// recurred in every migrate mirror.
func tableName(db *gorm.DB, name string) string {
	return db.NamingStrategy.TableName(name)
}

// Migrate applies the current schema to db via GORM AutoMigrate, then runs
// the phase ③ tenant_key post-migration (backfill + reconcile — the same
// procedure as deploy/phase3-storage-tenant-key.sql, so `make migrate` alone
// re-keys a pre-③ database; fresh DBs no-op).
//
// Single migration entry point for storage-service: the `migrate` subcommand
// (cmd/server) and embedders that inject a parent db (NewModule +
// option.WithDB) both call it, so tables are created regardless of how the
// service runs. pkg re-exports it as pkg.Migrate.
//
// AutoMigrate creates missing tables/columns/indexes but never drops unused
// ones — when a column is removed from a model, dev DBs are recreated via
// testcontainer rather than migrated in place.
//
// Embedders migrate on the parent db before constructing the module (where
// `storage` is their chosen import alias for this package):
//
//	storage.Migrate(parentDB)
//	hdl, err := storage.NewModule(cfg, option.WithDB(parentDB))
func Migrate(db *gorm.DB) error {
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	if err := postMigrateTenantKey(db); err != nil {
		return fmt.Errorf("post-migrate tenant_key: %w", err)
	}
	return nil
}

// postMigrateTenantKey mirrors deploy/phase3-storage-tenant-key.sql after
// AutoMigrate has added the columns/indexes (D-③4: add → backfill →
// reconcile). There is no superseded composite index to drop in storage —
// the dedup domain is the (immutable) key_prefix itself, and the new
// uniq_storage_apps_tenant_key index comes from the model tags. Rows written
// by pre-③ code during the deploy window are healed by the next run's
// backfill; rows with an empty app_key (pre-app era) can never be attributed
// and stay NULL on purpose.
func postMigrateTenantKey(db *gorm.DB) error {
	// QF1008 false positive: Dialector is an interface-typed field, Name is
	// its method — the selector cannot be removed.
	//nolint:staticcheck // gorm.DB.Dialector is an interface field, not embedding
	if db.Dialector.Name() != "postgres" {
		// Non-PG dev dialects (sqlite testcontainers are PG here; MySQL
		// deployments run the deploy SQL) — indexes already come from
		// AutoMigrate; nothing to backfill on a fresh DB.
		return nil
	}

	apps := tableName(db, "storage_apps")
	files := tableName(db, "storage_files")
	sessions := tableName(db, "storage_upload_sessions")

	// Backfill (idempotent: NULL rows only). apps map to their app_key
	// literal (the ③ window mapping); files/sessions map through their
	// stored app_key.
	if err := db.Exec(fmt.Sprintf(
		`UPDATE %s SET tenant_key = app_key WHERE tenant_key IS NULL`, apps)).Error; err != nil {
		return fmt.Errorf("backfill storage_apps: %w", err)
	}
	if err := db.Exec(fmt.Sprintf(`UPDATE %s f SET tenant_key = a.tenant_key
		FROM %s a
		WHERE f.app_key = a.app_key AND f.app_key <> '' AND f.tenant_key IS NULL`, files, apps)).Error; err != nil {
		return fmt.Errorf("backfill storage_files: %w", err)
	}
	if err := db.Exec(fmt.Sprintf(`UPDATE %s s SET tenant_key = a.tenant_key
		FROM %s a
		WHERE s.app_key = a.app_key AND s.app_key <> '' AND s.tenant_key IS NULL`, sessions, apps)).Error; err != nil {
		return fmt.Errorf("backfill storage_upload_sessions: %w", err)
	}

	// Reconcile: every row that can carry a tenant_key does. Empty-app_key
	// rows are exempt (pre-app era — no attribution possible). A non-empty
	// app_key matching no app row can never be healed — fail loudly so the
	// operator resolves it instead of silently drifting.
	if err := reconcileTenantKey(db, "storage_apps",
		fmt.Sprintf(`SELECT count(*), count(tenant_key) FROM %s`, apps)); err != nil {
		return err
	}
	if err := reconcileTenantKey(db, "storage_files",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE app_key <> ''), count(tenant_key) FILTER (WHERE app_key <> '') FROM %s`, files)); err != nil {
		return err
	}
	if err := reconcileTenantKey(db, "storage_upload_sessions",
		fmt.Sprintf(`SELECT count(*) FILTER (WHERE app_key <> ''), count(tenant_key) FILTER (WHERE app_key <> '') FROM %s`, sessions)); err != nil {
		return err
	}

	slog.Info("migrate: phase3 tenant_key post-migration complete")
	return nil
}

// reconcileTenantKey fails when filled != total for the given probe query.
func reconcileTenantKey(db *gorm.DB, table, probe string) error {
	var total, filled int64
	if err := db.Raw(probe).Row().Scan(&total, &filled); err != nil {
		return fmt.Errorf("reconcile %s: %w", table, err)
	}
	if filled != total {
		return fmt.Errorf("reconcile %s: %d of %d attributable rows carry tenant_key (rows whose app_key matches no storage_apps.app_key can never be healed)",
			table, filled, total)
	}
	slog.Info("migrate: phase3 tenant_key reconcile ok", "table", table, "total", total, "filled", filled)
	return nil
}
