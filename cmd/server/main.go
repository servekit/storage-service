// Package main is the entry point for the storage service server.
package main

import (
	"log/slog"
	"os"

	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/signalx"

	pkg "github.com/servekit/storage-service/pkg"
	"github.com/servekit/storage-service/pkg/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}
	logging.Setup(cfg.Log)

	srv, err := pkg.NewServer(cfg)
	if err != nil {
		slog.Error("init server", "error", err)
		os.Exit(1)
	}

	if err := signalx.RunWithForceQuit(srv); err != nil {
		slog.Error("run server", "error", err)
		os.Exit(1)
	}
}
