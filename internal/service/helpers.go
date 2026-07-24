package service

import (
	"fmt"
	"log/slog"

	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/thirdcall"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/ratelimit"
	"github.com/servekit/go-common/redisx"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// resolveDB returns the DB pool to use. If the caller injected one via WithDB,
// use it as-is (caller owns lifecycle). Otherwise build from cfg and register
// a Stopper on mgr so service.Stop closes it.
func resolveDB(cfg *config.Config, external *gorm.DB, mgr *lifecycle.Manager) (*gorm.DB, error) {
	if external != nil {
		return external, nil
	}
	db, err := dbx.New(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("init database: %w", err)
	}
	mgr.AddStopper("db", lifecycle.StopFunc(func() {
		sqlDB, err := db.DB()
		if err != nil {
			slog.Warn("get sql db for close", "error", err)
			return
		}
		if err := sqlDB.Close(); err != nil {
			slog.Warn("close db", "error", err)
		}
	}))
	return db, nil
}

// resolveGID returns the GID service to use. If injected, use as-is. Otherwise
// build from cfg; if the constructed instance exposes Close() error, register
// a Stopper so the underlying gRPC connection is released on shutdown.
func resolveGID(cfg *config.Config, external thirdcall.GIDService, mgr *lifecycle.Manager) (thirdcall.GIDService, error) {
	if external != nil {
		return external, nil
	}
	gid, err := thirdcall.NewGIDService(cfg.ThirdParty.GID)
	if err != nil {
		return nil, err
	}
	if closer, ok := gid.(interface{ Close() error }); ok {
		mgr.AddStopper("gid", lifecycle.StopFunc(func() {
			if err := closer.Close(); err != nil {
				slog.Warn("close gid", "error", err)
			}
		}))
	}
	return gid, nil
}

// resolveRedis returns the Redis client to use. If the caller injected one via
// WithRedis, use it as-is. Otherwise, if any Redis-dependent feature is
// configured (rate limit OR STS caching), build from cfg and register a
// Stopper on mgr.
//
// STS always constructs a *Service with a Redis client even when callers
// don't intend to use the cache (e.g. pre-signed URL flow only), and STS
// historically dereferenced rdb unconditionally. We build Redis whenever STS
// is configured so the cache works; STS now also nil-guards its cache path
// so a nil rdb degrades gracefully rather than panicking.
func resolveRedis(cfg *config.Config, external *redis.Client, mgr *lifecycle.Manager) (*redis.Client, error) {
	if external != nil {
		return external, nil
	}
	if !rateLimitConfigured(cfg.Storage.RateLimit) && !stsConfigured(cfg.Storage.STS) {
		return nil, nil
	}
	client, err := redisx.New(cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("init redis: %w", err)
	}
	mgr.AddStopper("redis", lifecycle.StopFunc(func() {
		if err := client.Close(); err != nil {
			slog.Warn("close redis", "error", err)
		}
	}))
	return client, nil
}

// --- internal helpers ---

// rateLimitConfigured reports whether a rate-limit config is meaningfully set.
// Per golang-development §14, configx (viper) always allocates a non-nil
// pointer even when the section is missing, so cfg == nil cannot detect "not
// configured". Instead we check the semantic content: rate limiting is active
// only when Global or per-route Rules carry at least one rule.
func rateLimitConfigured(cfg *ratelimit.Config) bool {
	return cfg != nil && (len(cfg.Global) > 0 || len(cfg.Rules) > 0)
}

// stsConfigured reports whether STS is meaningfully enabled. STS is active
// whenever the section is present (DefaultTTL/MaxTTL have non-zero defaults
// set by sts.New, so checking the pointer alone is sufficient — configx
// allocates a non-nil pointer only when the section exists in config).
func stsConfigured(cfg *config.STSConfig) bool {
	return cfg != nil
}
