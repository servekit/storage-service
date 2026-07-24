package main

import (
	"log/slog"
	"os"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"

	"github.com/servekit/storage-service/internal/store/models"
	"github.com/servekit/storage-service/pkg/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	logging.Setup(cfg.Log)

	db, err := dbx.New(cfg.Database)
	if err != nil {
		slog.Error("init database", "error", err)
		os.Exit(1)
	}

	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}
