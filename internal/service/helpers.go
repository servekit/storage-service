package service

import (
	"fmt"
	"log/slog"

	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/servekit/storage-service/internal/thirdcall/gid_service"
	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/option"

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

// resolveGID returns the GIDService. An injected handler
// (option.WithGIDHandler, set when a parent embeds this service) takes
// precedence over everything else and works even when cfg is nil (no
// ThirdParty.GID configured); the parent owns lifecycle, so no Stopper is
// registered in that path. Otherwise cfg must be set: grpc mode dials
// cfg.Target; module mode builds one from cfg.Config (standalone cmd/server).
// cfg.Config is gid-service's own *gidconfig.Config, so gidservice.NewModule
// consumes it directly and validates the snowflake fields at build time. grpc
// and self-built register a Stopper so mgr.Stop closes the grpc client / stops
// the Handler. The GIDService interface is internal.
func resolveGID(o *option.Options, cfg *config.RemoteServiceConfig[*gidconfig.Config], mgr *lifecycle.Manager) (gid_service.GIDService, error) {
	// Injected handler takes precedence (a parent shares its gid Handler),
	// even if cfg is nil (no ThirdParty.GID configured).
	if o.GIDHandler != nil {
		return gid_service.NewModule(o.GIDHandler, false), nil // borrowed; parent owns lifecycle
	}
	if cfg == nil {
		return nil, fmt.Errorf("third_party.gid: not configured")
	}
	switch cfg.Mode {
	case "grpc":
		gid, err := gid_service.NewGRPC(cfg.Target)
		if err != nil {
			return nil, fmt.Errorf("init gid-service: %w", err)
		}
		mgr.AddStopper("gid", lifecycle.StopFunc(func() {
			if err := gid.Close(); err != nil {
				slog.Warn("close gid-service", "error", err)
			}
		}))
		return gid, nil
	case "module":
		// o.GIDHandler is nil here (handled above); build from cfg.
		if cfg.Config == nil {
			return nil, fmt.Errorf("third_party.gid: module config required when no handler injected")
		}
		hdl, err := gidservice.NewModule(cfg.Config)
		if err != nil {
			return nil, fmt.Errorf("init gid-service: %w", err)
		}
		gid := gid_service.NewModule(hdl, true)
		mgr.AddStopper("gid", lifecycle.StopFunc(func() {
			if err := gid.Close(); err != nil {
				slog.Warn("close gid-service", "error", err)
			}
		}))
		return gid, nil
	default:
		return nil, fmt.Errorf("third_party.gid: unknown mode %q", cfg.Mode)
	}
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

// thirdPartyGID returns cfg.ThirdParty.GID without dereferencing a nil
// ThirdParty. Kept here (not in service.go) so gidconfig stays out of
// service.go's import list. resolveGID treats a nil return as "not configured".
func thirdPartyGID(cfg *config.Config) *config.RemoteServiceConfig[*gidconfig.Config] {
	if cfg.ThirdParty == nil {
		return nil
	}
	return cfg.ThirdParty.GID
}
