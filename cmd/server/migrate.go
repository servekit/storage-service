package main

import (
	"fmt"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"

	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"

	"gorm.io/gorm"
)

// runMigrate loads config and applies the current schema via GORM AutoMigrate.
// Operators (or CI) run this before bringing up the server, e.g.
// `docker run <image> migrate` or `./storage-service migrate`.
func runMigrate() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logging.Setup(cfg.Log)

	db, err := dbx.New(cfg.Database)
	if err != nil {
		return fmt.Errorf("init database: %w", err)
	}

	if err := runMigration(db); err != nil {
		return err
	}
	return nil
}

// runMigration applies the current schema via GORM AutoMigrate.
//
// AutoMigrate creates missing tables/columns/indexes but never drops unused
// ones — when a column is removed from a model, dev DBs are recreated via
// testcontainer rather than migrated in place.
func runMigration(db *gorm.DB) error {
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	return nil
}
