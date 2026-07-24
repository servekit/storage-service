# Upload Rate Limiting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Redis-backed fixed-window rate limiting to upload endpoints, with per-owner-type configurable limits.

**Architecture:** New `RateLimitConfig` in config maps proto enum strings (e.g. `OWNER_TYPE_USER`) to rate limit rules. `resolveRedis` helper (same pattern as `resolveDB`) creates or reuses a Redis client. `ratelimit.NewRedisLimiter` wraps it. Service checks `limiter.Allow()` at the entry of `generateUploadURL` and `getSTSCredential`.

**Tech Stack:** `go-common/redisx`, `go-common/ratelimit`, `go-redis/v9`

---

### Task 1: Add dependencies to go.mod

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Tidy imports to pull in redisx and ratelimit**

Run:
```bash
cd /Users/moss/code/base/storage-service && go get github.com/servekit/go-common/redisx github.com/servekit/go-common/ratelimit github.com/redis/go-redis/v9
```

- [ ] **Step 2: Verify module builds**

Run: `go build ./...`
Expected: success (no compile errors yet — we're just ensuring deps resolve)

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add redisx and ratelimit dependencies"
```

---

### Task 2: Add rate limit error code

**Files:**
- Modify: `pkg/xcodes/storage.go`

- [ ] **Step 1: Add `ErrRateLimited` to storage.go**

Add to the `var` block in `pkg/xcodes/storage.go`:

```go
ErrRateLimited = xerr.New("RATE_LIMITED", xerr.CategoryTooManyRequests, 429, "rate limit exceeded")
```

Note: verify the constant name for 429 in `go-common/xerr` categories. It may be `CategoryTooManyRequests` or similar. If it doesn't exist, use the raw int approach if supported, or pick the closest category.

- [ ] **Step 2: Verify build**

Run: `go build ./pkg/xcodes/`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add pkg/xcodes/storage.go
git commit -m "feat: add ErrRateLimited error code"
```

---

### Task 3: Extend config with Redis and RateLimit sections

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `config.example.yaml`

- [ ] **Step 1: Add config structs**

In `pkg/config/config.go`, add imports for `redisx` and `ratelimit`:

```go
import (
    "github.com/servekit/go-common/redisx"
    "github.com/servekit/go-common/ratelimit"
)
```

Add `Redis` field to `Config` struct (top-level, like `Database`):

```go
type Config struct {
    Server     ServerConfig
    Database   dbx.Config
    Redis      redisx.Config
    Storage    StorageConfig
    ThirdParty ThirdPartyConfig
    Log        logging.Config
}
```

Add `RateLimit` field to `StorageConfig`:

```go
type StorageConfig struct {
    UploadTokenTTL        time.Duration
    UploadTokenSecret     string
    DefaultQuotaBytes     int64
    DefaultBucket         string
    OrphanRetention       time.Duration
    SoftDeleteRetention   time.Duration
    DeletedOwnerRetention time.Duration
    RateLimit             *RateLimitConfig
    Providers             []ProviderConfig
}
```

Define the new config types:

```go
// RateLimitConfig holds rate limiting settings for upload operations.
type RateLimitConfig struct {
    Prefix string                      `mapstructure:"prefix"`
    Rules  map[string][]RateLimitRule  `mapstructure:"rules"`
}

// RateLimitRule defines a single rate limit window.
type RateLimitRule struct {
    Window time.Duration `mapstructure:"window"`
    Max    int64         `mapstructure:"max"`
}
```

Add a method to convert `RateLimitConfig` to `ratelimit.Config`, validating enum keys:

```go
// ToRatelimitConfig validates enum string keys and converts to ratelimit.Config.
// Panics on invalid enum keys to fail fast at startup.
func (c *RateLimitConfig) ToRatelimitConfig() ratelimit.Config {
    rules := make(map[string][]ratelimit.Rule, len(c.Rules))
    for key, rs := range c.Rules {
        val, ok := storagev1.OwnerType_value[key]
        if !ok || val == 0 {
            panic(fmt.Sprintf("rate_limit: invalid owner type enum key %q", key))
        }
        ratelimitRules := make([]ratelimit.Rule, len(rs))
        for i, r := range rs {
            ratelimitRules[i] = ratelimit.Rule{Window: r.Window, Max: r.Max}
        }
        rules[key] = ratelimitRules
    }
    return ratelimit.Config{
        Prefix: c.Prefix,
        Rules:  rules,
    }
}
```

Add `storagev1` import for the `OwnerType_value` map:
```go
storagev1 "storage-service/gen/storage/v1"
```

- [ ] **Step 2: Update config.example.yaml**

Append `redis` section at the top level and `rate_limit` under `storage`:

```yaml
redis:
  addr: "localhost:6379"
  password: ""
  db: 0

storage:
  # ... existing fields ...
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

- [ ] **Step 3: Verify build**

Run: `go build ./pkg/config/`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add pkg/config/config.go config.example.yaml
git commit -m "feat: add Redis and RateLimit config sections"
```

---

### Task 4: Add WithRedis option

**Files:**
- Modify: `pkg/option/option.go`

- [ ] **Step 1: Add Redis field and WithRedis constructor**

Add import:
```go
"github.com/redis/go-redis/v9"
```

Add to `Options` struct:
```go
type Options struct {
    DB         *gorm.DB
    Redis      *redis.Client
    GIDService thirdcall.GIDService
}
```

Add constructor:
```go
// WithRedis provides an existing Redis connection.
func WithRedis(client *redis.Client) Option {
    return func(o *Options) { o.Redis = client }
}
```

- [ ] **Step 2: Verify build**

Run: `go build ./pkg/option/`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add pkg/option/option.go
git commit -m "feat: add WithRedis option"
```

---

### Task 5: Wire Redis + limiter into service

**Files:**
- Modify: `internal/service/service.go`
- Modify: `internal/service/helpers.go`

- [ ] **Step 1: Add limiter field and ownRedis to StorageService**

In `internal/service/service.go`, add imports:

```go
"github.com/servekit/go-common/ratelimit"
"github.com/servekit/go-common/redisx"
"github.com/redis/go-redis/v9"
```

Add fields to `StorageService`:

```go
type StorageService struct {
    storagev1.UnimplementedStorageServiceServer

    db       *gorm.DB
    ownDB    bool
    redis    *redis.Client
    ownRedis bool
    registry *provider.Registry
    gid      thirdcall.GIDService
    limiter  ratelimit.Limiter
    cfg      *config.Config

    objectRepo *repository.ObjectRepo
    fileRepo   *repository.FileRepo
}
```

Update `New()` to resolve Redis and build limiter. Insert after GID resolution:

```go
redisClient, ownRedis, err := resolveRedis(cfg, o.Redis)
if err != nil {
    return nil, err
}

var limiter ratelimit.Limiter
if redisClient != nil && cfg.Storage.RateLimit != nil {
    rlCfg := cfg.Storage.RateLimit.ToRatelimitConfig()
    limiter = ratelimit.NewRedisLimiter(redisClient, rlCfg)
}
```

Update the return statement to include new fields:

```go
return &StorageService{
    db:         db,
    ownDB:      ownDB,
    redis:      redisClient,
    ownRedis:   ownRedis,
    registry:   registry,
    gid:        gidGen,
    limiter:    limiter,
    cfg:        cfg,
    objectRepo: objectRepo,
    fileRepo:   fileRepo,
}, nil
```

- [ ] **Step 2: Update Close() to release Redis**

In `service.go`, update `Close()`:

```go
func (s *StorageService) Close() {
    if s.ownDB && s.db != nil {
        sqlDB, err := s.db.DB()
        if err == nil {
            _ = sqlDB.Close()
        }
    }
    if s.ownRedis && s.redis != nil {
        _ = s.redis.Close()
    }
}
```

- [ ] **Step 3: Add resolveRedis helper**

In `internal/service/helpers.go`, add import for `redisx`:

```go
"github.com/servekit/go-common/redisx"
"github.com/redis/go-redis/v9"
```

Add the helper function at the bottom:

```go
func resolveRedis(cfg *config.Config, external *redis.Client) (*redis.Client, bool, error) {
    if external != nil {
        return external, false, nil
    }
    if cfg.Storage.RateLimit == nil {
        return nil, false, nil
    }
    client, err := redisx.New(&cfg.Redis)
    if err != nil {
        return nil, false, fmt.Errorf("init redis: %w", err)
    }
    return client, true, nil
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: success

- [ ] **Step 5: Commit**

```bash
git add internal/service/service.go internal/service/helpers.go
git commit -m "feat: wire Redis client and rate limiter into service"
```

---

### Task 6: Add rate limit checks to upload methods

**Files:**
- Modify: `internal/service/upload.go`

- [ ] **Step 1: Add checkUploadRateLimit helper**

Add to `upload.go` (or `helpers.go` — since it's upload-specific, `upload.go` is fine):

```go
func (s *StorageService) checkUploadRateLimit(ownerType int32, ownerID int64) error {
    if s.limiter == nil {
        return nil
    }
    enumName, ok := storagev1.OwnerType_name[ownerType]
    if !ok {
        return xcodes.ErrInternal.Newf("unknown owner type: %d", ownerType)
    }
    purpose := "upload:" + enumName
    allowed, err := s.limiter.Allow(purpose, strconv.FormatInt(ownerID, 10))
    if err != nil {
        return xcodes.ErrInternal.Wrap(err)
    }
    if !allowed {
        return xcodes.ErrRateLimited.New()
    }
    return nil
}
```

Add `strconv` to imports in the file where this helper lives.

- [ ] **Step 2: Add rate limit call in generateUploadURL**

At the top of `generateUploadURL`, after extracting `ownerType` and `ownerID` (lines 19-20), add:

```go
if err := s.checkUploadRateLimit(ownerType, ownerID); err != nil {
    return nil, err
}
```

- [ ] **Step 3: Add rate limit call in getSTSCredential**

At the top of `getSTSCredential`, after extracting `ownerType` and `ownerID` (lines 213-214), add:

```go
if err := s.checkUploadRateLimit(ownerType, ownerID); err != nil {
    return nil, err
}
```

- [ ] **Step 4: Verify build**

Run: `go build ./...`
Expected: success

- [ ] **Step 5: Commit**

```bash
git add internal/service/upload.go
git commit -m "feat: add rate limit checks to generateUploadURL and getSTSCredential"
```

---

### Task 7: Write unit tests

**Files:**
- Create: `internal/service/upload_ratelimit_test.go`

- [ ] **Step 1: Write rate limit tests**

Create test file using miniredis via `redisx.NewTestClient(t)`:

```go
package service

import (
    "context"
    "testing"
    "time"

    "github.com/servekit/go-common/redisx"
    "github.com/servekit/go-common/ratelimit"
    storagev1 "storage-service/gen/storage/v1"
    "storage-service/pkg/xcodes"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestCheckUploadRateLimit_noLimiter(t *testing.T) {
    svc := &StorageService{limiter: nil}
    err := svc.checkUploadRateLimit(1, 123)
    assert.NoError(t, err)
}

func TestCheckUploadRateLimit_allowed(t *testing.T) {
    client := redisx.NewTestClient(t)
    limiter := ratelimit.NewRedisLimiter(client, ratelimit.Config{
        Prefix: "test:ratelimit",
        Rules: map[string][]ratelimit.Rule{
            "upload:OWNER_TYPE_USER": {{Window: time.Minute, Max: 5}},
        },
    })
    svc := &StorageService{limiter: limiter}

    err := svc.checkUploadRateLimit(int32(storagev1.OwnerType_OWNER_TYPE_USER), 123)
    assert.NoError(t, err)
}

func TestCheckUploadRateLimit_exceeded(t *testing.T) {
    client := redisx.NewTestClient(t)
    limiter := ratelimit.NewRedisLimiter(client, ratelimit.Config{
        Prefix: "test:ratelimit",
        Rules: map[string][]ratelimit.Rule{
            "upload:OWNER_TYPE_USER": {{Window: time.Minute, Max: 1}},
        },
    })
    svc := &StorageService{limiter: limiter}

    // First request allowed
    err := svc.checkUploadRateLimit(int32(storagev1.OwnerType_OWNER_TYPE_USER), 123)
    require.NoError(t, err)

    // Second request blocked
    err = svc.checkUploadRateLimit(int32(storagev1.OwnerType_OWNER_TYPE_USER), 123)
    assert.ErrorIs(t, err, xcodes.ErrRateLimited)
}

func TestCheckUploadRateLimit_differentOwners(t *testing.T) {
    client := redisx.NewTestClient(t)
    limiter := ratelimit.NewRedisLimiter(client, ratelimit.Config{
        Prefix: "test:ratelimit",
        Rules: map[string][]ratelimit.Rule{
            "upload:OWNER_TYPE_USER": {{Window: time.Minute, Max: 1}},
        },
    })
    svc := &StorageService{limiter: limiter}

    // First user exhausts limit
    err := svc.checkUploadRateLimit(int32(storagev1.OwnerType_OWNER_TYPE_USER), 1)
    require.NoError(t, err)

    // Same user blocked
    err = svc.checkUploadRateLimit(int32(storagev1.OwnerType_OWNER_TYPE_USER), 1)
    assert.ErrorIs(t, err, xcodes.ErrRateLimited)

    // Different user allowed (different target)
    err = svc.checkUploadRateLimit(int32(storagev1.OwnerType_OWNER_TYPE_USER), 2)
    assert.NoError(t, err)
}

func TestCheckUploadRateLimit_unknownOwnerType(t *testing.T) {
    client := redisx.NewTestClient(t)
    limiter := ratelimit.NewRedisLimiter(client, ratelimit.Config{
        Prefix: "test:ratelimit",
        Rules:  map[string][]ratelimit.Rule{},
    })
    svc := &StorageService{limiter: limiter}

    err := svc.checkUploadRateLimit(99, 123)
    assert.ErrorIs(t, err, xcodes.ErrInternal)
}
```

Note: `ErrorIs` checks require `xcodes.ErrRateLimited` to implement the `Is()` method (it does via `go-common/xerr`). If the test pattern in this project uses a different assertion style for xerr errors, adapt accordingly.

- [ ] **Step 2: Run tests**

Run: `go test -race ./internal/service/ -run TestCheckUploadRateLimit -v`
Expected: all 5 tests pass

- [ ] **Step 3: Commit**

```bash
git add internal/service/upload_ratelimit_test.go
git commit -m "test: add rate limit unit tests"
```

---

### Task 8: Verify full test suite and lint

**Files:** None (verification only)

- [ ] **Step 1: Run full test suite**

Run: `go test -race ./...`
Expected: all tests pass

- [ ] **Step 2: Run linter**

Run: `golangci-lint run ./...`
Expected: no new warnings

- [ ] **Step 3: Run goimports/gofmt**

Run:
```bash
gofmt -l . && goimports -l .
```
Expected: no output (all files formatted)
