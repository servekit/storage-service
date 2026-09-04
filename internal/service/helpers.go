package service

import (
	"fmt"

	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/servekit/storage-service/pkg/config"
	"github.com/servekit/storage-service/pkg/option"

	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/ratelimit"
	"github.com/servekit/go-common/redisx"

	"github.com/redis/go-redis/v9"
)

// resolveGID returns the gid dependency. Construction delegates to
// gidservice.Connect, which owns the mode switch and lifecycle registration;
// only the adoption of a parent-injected Handler stays here — it reads this
// service's own options and the parent owns that lifecycle.
func resolveGID(o *option.Options, cfg *config.RemoteServiceConfig[*gidconfig.Config], mgr *lifecycle.Manager) (gidservice.Service, error) {
	// Injected handler takes precedence (a parent shares its gid Handler),
	// even if cfg is nil (no ThirdParty.GID configured).
	if o.GIDHandler != nil {
		return o.GIDHandler, nil // borrowed; parent owns lifecycle
	}
	if cfg == nil {
		return nil, fmt.Errorf("third_party.gid: not configured")
	}
	gid, _, err := gidservice.Connect(gidservice.ConnectConfig{
		Mode:   cfg.Mode,
		Target: cfg.Target,
		Config: cfg.Config,
	}, mgr)
	return gid, err
}

// resolveRedis returns the Redis client to use. If the caller injected one via
// WithRedis, use it as-is. Otherwise, if any Redis-dependent feature is
// configured (rate limit OR STS caching), build from cfg and register a
// Stopper on mgr via redisx.Connect.
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
	return redisx.Connect(cfg.Redis, nil, mgr)
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
