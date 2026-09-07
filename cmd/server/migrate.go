package main

import (
	"fmt"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"

	pkg "github.com/servekit/storage-service/pkg"
	"github.com/servekit/storage-service/pkg/config"
)

// runMigrate loads config and applies the current schema via pkg.Migrate.
// Operators (or CI) run this before bringing up the server, e.g.
// `docker run <image> migrate` or `./storage-service migrate`.
//
// `migrate --seed-from-config` additionally imports legacy YAML providers /
// buckets / settings into the DB platform tables (one-shot; existing names
// skipped). After seeding, remove the providers block from YAML.
//
// pkg.Migrate is the same entry point embedders call on an injected db, so
// standalone and in-process module deployments create tables identically.
func runMigrate(seed bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logging.Setup(cfg.Log)

	db, err := dbx.New(cfg.Database)
	if err != nil {
		return fmt.Errorf("init database: %w", err)
	}

	if err := pkg.Migrate(db); err != nil {
		return err
	}
	if seed {
		if err := pkg.SeedFromConfig(db, cfg); err != nil {
			return fmt.Errorf("seed from config: %w", err)
		}
	}
	return nil
}
