// Package sts issues short-lived cloud-provider credentials (STS) for the
// storage service. Credentials are cached in Redis per (owner, vendor, bucket)
// with singleflight semantics so concurrent misses for the same key share one
// issuer call instead of hammering the cloud provider.
package sts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/servekit/go-common/jsonx"
	"github.com/servekit/go-common/redisx"

	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/pkg/config"

	"github.com/redis/go-redis/v9"
)

// Issuer abstracts the source of STS credentials so the service can be
// unit-tested without a real cloud provider and shared across domains
// (upload, admin, ...).
type Issuer interface {
	Issue(ctx context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error)
}

// FuncIssuer adapts a function to the Issuer interface. Call sites build one
// to bridge their own provider/registry without forcing this package to
// import it.
type FuncIssuer func(ctx context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error)

// Service issues STS credentials, backed by a Redis cache for deduplication
// across concurrent callers. The cache key is (ownerType, ownerID, vendor,
// bucket); a Redis SETNX lock provides singleflight semantics so concurrent
// misses for the same key share one issuer call instead of hammering the
// cloud provider.
type Service struct {
	rdb    *redis.Client
	issuer Issuer
	cfg    *config.STSConfig
	lock   *redisx.Lock
}

// lostLockBackoffs paces cache-read retries when this caller lost the dedup
// race. Total worst-case wait: 50+100+200 = 350ms before giving up.
var lostLockBackoffs = []time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
}

// Issue implements Issuer.
func (f FuncIssuer) Issue(ctx context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
	return f(ctx, policy)
}

// New constructs an STS service. cfg comes straight from pkg/config (no
// duplicate Config/LockConfig types here — reuses storage-service's schema).
// Zero-value fields fall back to safe defaults (DefaultTTL=15m, MaxTTL=1h,
// SafetyMargin=5m, ReadRefreshWindow=30s, lock Prefix/TTL/Tries/Wait). The
// singleflight dedup lock is constructed once here and shared across all Get
// calls. Semantic consistency (SafetyMargin < MaxTTL, ReadRefreshWindow <
// SafetyMargin) is enforced by Config.Validate at load time; the fallbacks
// here only cover the case of tests constructing STSConfig{} directly.
func New(rdb *redis.Client, issuer Issuer, cfg *config.STSConfig) *Service {
	if cfg == nil {
		cfg = &config.STSConfig{}
	}
	if cfg.DefaultTTL == 0 {
		cfg.DefaultTTL = 15 * time.Minute
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = time.Hour
	}
	if cfg.SafetyMargin == 0 {
		cfg.SafetyMargin = 5 * time.Minute
	}
	if cfg.ReadRefreshWindow == 0 {
		cfg.ReadRefreshWindow = 30 * time.Second
	}
	// Fallbacks for lock config in case the caller didn't set via pkg/config
	// defaults — e.g., tests constructing STSConfig{} directly. cfg.Lock may
	// be nil (pointer per §14), so allocate a fresh zero-value STSLockConfig.
	lockCfg := cfg.Lock
	if lockCfg == nil {
		lockCfg = &config.STSLockConfig{}
		cfg.Lock = lockCfg
	}
	if lockCfg.Prefix == "" {
		lockCfg.Prefix = "sts:lock"
	}
	if lockCfg.TTL == 0 {
		lockCfg.TTL = 10 * time.Second
	}
	if lockCfg.Tries == 0 {
		lockCfg.Tries = 3
	}
	if lockCfg.Wait == 0 {
		lockCfg.Wait = 50 * time.Millisecond
	}
	// The lock is shared across all Get calls so concurrent misses dedup against
	// a single SETNX key per (owner, vendor, bucket). If construction fails (rare
	// misconfig), the service still works but loses singleflight dedup — log and
	// continue with a nil lock (Get checks for nil before acquiring).
	lock, err := redisx.NewLock(rdb, &redisx.LockConfig{
		Prefix: lockCfg.Prefix,
		TTL:    lockCfg.TTL,
		Tries:  lockCfg.Tries,
		Wait:   lockCfg.Wait,
	})
	if err != nil {
		slog.Warn("sts: construct dedup lock, singleflight disabled", "error", err)
	}
	return &Service{rdb: rdb, issuer: issuer, cfg: cfg, lock: lock}
}

// ResolveTTL clamps the caller-provided ttl into [0, MaxTTL]; 0 → DefaultTTL.
// Exported so callers can align session/token/policy TTLs on a single value
// (the service would otherwise silently substitute its default for ttl=0).
func (s *Service) ResolveTTL(ttl time.Duration) time.Duration {
	if ttl == 0 {
		return s.cfg.DefaultTTL
	}
	if ttl > s.cfg.MaxTTL {
		return s.cfg.MaxTTL
	}
	return ttl
}

// Get returns an STS credential for the given owner/vendor/bucket, serving
// from cache when fresh and fetching from the issuer otherwise.
// Concurrency: uses Redis SETNX-based lock (singleflight pattern) to prevent
// multiple concurrent misses from hammering the issuer.
//
// If Redis is not configured (s.rdb == nil — e.g. rate limit off and STS on,
// see internal/service/helpers.resolveRedis), Get bypasses the cache and
// the singleflight lock and invokes the issuer directly on every call.
func (s *Service) Get(ctx context.Context, ownerType int32, ownerID int64, vendor int32, bucket string, ttl time.Duration, policy *storage.STSPolicy) (*storage.STSCredential, error) {
	resolvedTTL := s.ResolveTTL(ttl)

	// No-Redis fast path: skip cache + lock and fetch directly. Cache read
	// would dereference s.rdb and panic.
	if s.rdb == nil {
		return s.fetchAndCache(ctx, "", resolvedTTL, policy)
	}

	key := cacheKey(ownerType, ownerID, vendor, bucket)

	// Fast path: cache hit.
	if cached, err := s.read(ctx, key); err == nil && cached != nil {
		return cached, nil
	}

	// Slow path: acquire lock to dedupe concurrent fetches.
	// If the dedup lock failed to construct (nil), skip singleflight and fetch
	// directly — the cache read above already missed.
	if s.lock == nil {
		return s.fetchAndCache(ctx, key, resolvedTTL, policy)
	}

	id, acquireErr := s.lock.Acquire(ctx, lockTargetFromCacheKey(key))
	if acquireErr != nil {
		// Lost the race: wait for the winner to populate the cache. We must NOT
		// fall through to fetchAndCache here — the winner is still in flight and a
		// second issuer call would defeat the singleflight (2 concurrent cloud
		// calls). Return an error so the caller can decide to retry Get on its own
		// schedule, or accept the failure.
		for _, wait := range lostLockBackoffs {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if cached, err := s.read(ctx, key); err == nil && cached != nil {
				return cached, nil
			}
		}
		// Winner is still in flight (slow cloud call). Give up rather than issue
		// a duplicate call.
		return nil, fmt.Errorf("sts: credential not yet cached after losing dedup race (winner still in flight)")
	}
	// Lock auto-releases after TTL. Release failure is benign (worst case:
	// the lock lingers up to TTL=10s) but per CLAUDE.md we log it rather than
	// discard via _ = (only Close() cleanup qualifies for silent suppression).
	// lua CAS in Release prevents releasing another holder's lock.
	defer func() {
		if err := s.lock.Release(ctx, lockTargetFromCacheKey(key), id); err != nil {
			slog.Warn("sts: release dedup lock", "error", err)
		}
	}()
	// Double-check after acquiring lock.
	if cached, err := s.read(ctx, key); err == nil && cached != nil {
		return cached, nil
	}

	return s.fetchAndCache(ctx, key, resolvedTTL, policy)
}

// fetchAndCache invokes the issuer and caches the result.
func (s *Service) fetchAndCache(ctx context.Context, key string, resolvedTTL time.Duration, policy *storage.STSPolicy) (*storage.STSCredential, error) {
	creds, err := s.issuer.Issue(ctx, policy)
	if err != nil {
		return nil, fmt.Errorf("get STS token: %w", err)
	}
	// Cap cache TTL at the credential's actual expiration minus SafetyMargin,
	// so any cache hit hands out a credential with at least SafetyMargin of
	// usable life at the cloud provider. Skip clamping if ExpiresAt is zero
	// (provider bug or unimplemented): time.Until(time.Time{}) returns ~-1759
	// years, which would yield a negative TTL that Redis rejects — defeating
	// the cache entirely.
	//
	// Config.Validate enforces SafetyMargin < MaxTTL at startup, so
	// credTTL - SafetyMargin stays positive for any cred the issuer actually
	// returns with lifetime >= MaxTTL. read() defends the residual edge case
	// (cred already inside ReadRefreshWindow) by treating it as a miss.
	cacheTTL := resolvedTTL
	if !creds.ExpiresAt.IsZero() {
		if credTTL := time.Until(creds.ExpiresAt) - s.cfg.SafetyMargin; credTTL > 0 && credTTL < cacheTTL {
			cacheTTL = credTTL
		}
	}
	// Best-effort cache write: a Redis blip must not deny the caller a valid
	// credential — on failure the next Get() simply re-fetches. But unlike
	// resource cleanup (Close), a cache write failure is not benign: it may
	// indicate a Redis issue, so we log it rather than discard the error
	// (CLAUDE.md forbids _ = except for cleanup).
	//
	// Skip entirely when Redis is not configured: write would panic.
	if s.rdb == nil {
		return creds, nil
	}
	if err := s.write(ctx, key, creds, cacheTTL); err != nil {
		slog.Warn("sts: cache write failed, next Get will re-fetch", "key", key, "error", err)
	}
	return creds, nil
}

func (s *Service) read(ctx context.Context, key string) (*storage.STSCredential, error) {
	raw, err := s.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	var creds storage.STSCredential
	if err := jsonx.Unmarshal(raw, &creds); err != nil {
		return nil, err
	}
	// Defense in depth for the safety margin: even if the Redis key still has
	// TTL left, the credential itself may have slipped into the refresh window
	// (e.g. issuer returned a very short-lived cred that bypassed the clamp in
	// fetchAndCache, or wall-clock advanced between write and read). Treat
	// near-expiry as a miss so the caller re-issues instead of receiving a
	// token the cloud provider will reject.
	//
	// ReadRefreshWindow is configured separately from SafetyMargin because
	// read does not know the cred's original resolvedTTL — only its current
	// remaining lifetime. Config.Validate enforces ReadRefreshWindow <
	// SafetyMargin so this stays strictly a last-resort check.
	if !creds.ExpiresAt.IsZero() && time.Until(creds.ExpiresAt) <= s.cfg.ReadRefreshWindow {
		return nil, nil
	}
	return &creds, nil
}

func (s *Service) write(ctx context.Context, key string, creds *storage.STSCredential, ttl time.Duration) error {
	raw, err := jsonx.Marshal(creds)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, key, raw, ttl).Err()
}

// --- internal helpers ---

// cacheKey builds the Redis cache key for an STS credential.
// vendor is in the key as future-proofing: today buckets are vendor-pinned
// at the registry level, but if that ever changes, vendor-scoped cache slots
// prevent cross-vendor credential confusion.
// bucket is DNS-safe per provider validation, so no escaping needed.
func cacheKey(ownerType int32, ownerID int64, vendor int32, bucket string) string {
	return fmt.Sprintf("sts:cache:%d:%d:%d:%s", ownerType, ownerID, vendor, bucket)
}

// lockTargetFromCacheKey derives the lock target from a cache key.
// Both share the same suffix so cache miss + lock acquisition for the same
// (owner, vendor, bucket) are correlated.
func lockTargetFromCacheKey(cacheKey string) string {
	return strings.TrimPrefix(cacheKey, "sts:cache:")
}
