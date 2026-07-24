# Upload Rate Limiting Design

## Goal

Prevent upload abuse by rate-limiting `GenerateUploadURL` and `GetSTSCredential` per owner, using Redis-backed fixed-window counters from `go-common/ratelimit`.

## Configuration

Two new config sections in YAML:

```yaml
redis:
  addr: "localhost:6379"
  password: ""
  db: 0

storage:
  rate_limit:
    prefix: "storage:ratelimit"
    rules:
      OWNER_TYPE_USER:
        - window: 1m
          max: 10
        - window: 1h
          max: 200
      OWNER_TYPE_BUSINESS:
        - window: 1m
          max: 100
        - window: 1h
          max: 2000
```

- `Redis` field uses `go-common/redisx.Config`.
- `RateLimit.Prefix` is the Redis key namespace.
- `RateLimit.Rules` keys are proto enum strings (e.g. `OWNER_TYPE_USER`). Validated at init via `storagev1.OwnerType_value` map — invalid key panics immediately.
- If `rate_limit` is not configured, Redis and limiter are not initialized (backward compatible, no Redis dependency unless opted in).

## Dependency Injection

```
[WithRedis] or config.Redis → redisx.New() → *redis.Client
                                              ↓
                                ratelimit.NewRedisLimiter(client, rateLimitCfg)
                                              ↓
                                StorageService.limiter (ratelimit.Limiter interface)
```

- `option.Options` gains `Redis *redis.Client` field, with `WithRedis(client)` option (same pattern as `WithDB`).
- `service.New()` resolves Redis: uses injected client if provided, otherwise creates one from `cfg.Redis` when `cfg.Storage.RateLimit` is non-nil.
- `StorageService` gains `limiter ratelimit.Limiter` field (nil = no rate limiting). Limiter is built from the resolved Redis client + rate limit config.
- `StorageService.Close()` closes the Redis client if the service owns it (same pattern as DB).

## Rate Limit Check

Applied at the entry of `generateUploadURL` and `getSTSCredential` (before any business logic):

```go
if s.limiter != nil {
    purpose := fmt.Sprintf("upload:%s", ownerType.String())
    ok, err := s.limiter.Allow(purpose, strconv.FormatInt(ownerID, 10))
    if err != nil {
        return nil, xcodes.ErrInternal.Wrap(err)
    }
    if !ok {
        return nil, xcodes.ErrRateLimited.New()
    }
}
```

`confirmUpload` is NOT rate-limited (it finalizes an already-initiated upload).

## Error Code

New error code in `internal/xcodes/`: `ErrRateLimited` mapped to HTTP 429.

## Files Changed

| File | Change |
|------|--------|
| `pkg/config/config.go` | Add `Redis redisx.Config`, `RateLimitConfig` to `Config`/`StorageConfig` |
| `pkg/option/option.go` | Add `Redis *redis.Client` field, add `WithRedis()` |
| `internal/service/service.go` | Add `limiter` field, resolve Redis, build limiter in `New()` |
| `internal/service/upload.go` | Add rate limit check in `generateUploadURL`, `getSTSCredential` |
| `internal/xcodes/upload.go` | Add `ErrRateLimited` |
| `config.example.yaml` | Add `redis` and `rate_limit` example |
| `go.mod` | Add `go-redis`, `go-common/redisx`, `go-common/ratelimit` |

## What This Does NOT Do

- No rate limiting on download, file management, or admin endpoints.
- No sliding window (fixed window is sufficient for abuse prevention).
- No distributed coordination beyond Redis counters.
