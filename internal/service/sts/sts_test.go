package sts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/servekit/go-common/jsonx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/redisx"

	"github.com/servekit/storage-service/internal/provider/storage"
	"github.com/servekit/storage-service/pkg/config"
)

// fakeIssuer counts calls and returns predictable creds.
type fakeIssuer struct {
	calls  int
	creds  *storage.STSCredential
	err    error
	policy *storage.STSPolicy
}

func (f *fakeIssuer) Issue(_ context.Context, policy *storage.STSPolicy) (*storage.STSCredential, error) {
	f.calls++
	f.policy = policy
	if f.err != nil {
		return nil, f.err
	}
	return f.creds, nil
}

// TestSTS_HitSkipsIssuer verifies second call within TTL doesn't invoke issuer.
func TestSTS_HitSkipsIssuer(t *testing.T) {
	rdb := redisx.NewTestClient(t)
	issuer := &fakeIssuer{creds: &storage.STSCredential{AccessKey: "ak-1", ExpiresAt: time.Now().Add(time.Hour)}}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	c1, err := svc.Get(context.Background(), 1, 2, 1, "bucket-a", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-a"})
	require.NoError(t, err)
	assert.Equal(t, "ak-1", c1.AccessKey)

	c2, err := svc.Get(context.Background(), 1, 2, 1, "bucket-a", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-a"})
	require.NoError(t, err)
	assert.Equal(t, "ak-1", c2.AccessKey)
	assert.Equal(t, 1, issuer.calls, "issuer should be called once; cache hit on second call")
}

// TestSTS_TTLClamped verifies ttl > MaxTTL is clamped to MaxTTL.
func TestSTS_TTLClamped(t *testing.T) {
	rdb := redisx.NewTestClient(t)
	issuer := &fakeIssuer{creds: &storage.STSCredential{AccessKey: "ak-clamp", ExpiresAt: time.Now().Add(24 * time.Hour)}}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: 30 * time.Second})

	// Caller asks for 10 minutes; MaxTTL is 30s, so cache TTL should be 30s.
	_, err := svc.Get(context.Background(), 1, 2, 1, "bucket-clamp", 10*time.Minute, &storage.STSPolicy{Bucket: "bucket-clamp"})
	require.NoError(t, err)

	key := cacheKey(1, 2, 1, "bucket-clamp")
	ttl, err := rdb.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	assert.InDelta(t, 30*time.Second, ttl, float64(2*time.Second), "cache TTL should be clamped to MaxTTL")
	assert.Less(t, ttl, 30*time.Second+time.Second)
	assert.Greater(t, ttl, 28*time.Second)
}

// TestSTS_PolicyUnchanged verifies ttl = 0 resolves to DefaultTTL.
func TestSTS_PolicyUnchanged(t *testing.T) {
	rdb := redisx.NewTestClient(t)
	issuer := &fakeIssuer{creds: &storage.STSCredential{AccessKey: "ak-default", ExpiresAt: time.Now().Add(time.Hour)}}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 20 * time.Minute, MaxTTL: time.Hour})

	// Caller passes ttl = 0, expect resolved = DefaultTTL.
	_, err := svc.Get(context.Background(), 1, 2, 1, "bucket-default", 0, &storage.STSPolicy{Bucket: "bucket-default"})
	require.NoError(t, err)

	key := cacheKey(1, 2, 1, "bucket-default")
	ttl, err := rdb.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	assert.InDelta(t, 20*time.Minute, ttl, float64(2*time.Second), "cache TTL should equal DefaultTTL when caller passes 0")
	assert.Less(t, ttl, 20*time.Minute+time.Second)
	assert.Greater(t, ttl, 19*time.Minute)
}

// TestSTS_LoserRetryOnContention verifies the loser-backoff retry loop:
// when two concurrent Get calls race, the loser waits with backoff for the
// winner to populate the cache, rather than calling the issuer a second time.
func TestSTS_LoserRetryOnContention(t *testing.T) {
	rdb := redisx.NewTestClient(t)

	// Issuer blocks until releaseCh is closed, simulating slow cloud call.
	releaseCh := make(chan struct{})
	issuer := &blockingIssuer{
		releaseCh: releaseCh,
		creds:     &storage.STSCredential{AccessKey: "ak-block", ExpiresAt: time.Now().Add(time.Hour)},
	}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	var wg sync.WaitGroup
	results := make([]*storage.STSCredential, 2)
	errs := make([]error, 2)
	startCh := make(chan struct{})

	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			creds, err := svc.Get(context.Background(), 1, 2, 1, "bucket-x", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-x"})
			results[i] = creds
			errs[i] = err
		}()
	}

	close(startCh) // fire both goroutines concurrently

	// Give them time to race for the lock.
	time.Sleep(100 * time.Millisecond)
	close(releaseCh) // unblock the issuer (winner proceeds)

	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, "ak-block", results[0].AccessKey)
	assert.Equal(t, "ak-block", results[1].AccessKey)
	assert.Equal(t, 1, issuer.calls, "issuer must be called exactly once; loser should have hit cache via backoff retry")
}

// TestSTS_ZeroExpirationNoClamp verifies that a credential with zero
// ExpiresAt (provider bug or unimplemented) does NOT cause negative TTL.
// The cache should fall back to resolvedTTL without crashing.
func TestSTS_ZeroExpirationNoClamp(t *testing.T) {
	rdb := redisx.NewTestClient(t)
	issuer := &fakeIssuer{creds: &storage.STSCredential{AccessKey: "ak-zero"}} // ExpiresAt is zero
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	c1, err := svc.Get(context.Background(), 1, 2, 1, "bucket-zero", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-zero"})
	require.NoError(t, err)
	assert.Equal(t, "ak-zero", c1.AccessKey)

	// Verify cache was actually populated (positive TTL).
	key := cacheKey(1, 2, 1, "bucket-zero")
	ttl, err := rdb.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	assert.Positive(t, int64(ttl), "TTL must be positive even when ExpiresAt is zero")
}

// TestSTS_NilRedisDoesNotPanic covers the deployment where rate limiting is
// off (so resolveRedis returns nil) but STS is still in use. The STS service
// must fall through to direct issuer invocation and skip cache read/write,
// not dereference a nil *redis.Client.
//
// Regression: helpers.resolveRedis only triggers Redis construction when
// rate-limit config is non-empty; service.go then passes that nil into
// upload.New → sts.New. sts.Service.Get unconditionally called s.rdb.Get,
// which panicked.
func TestSTS_NilRedisDoesNotPanic(t *testing.T) {
	issuer := &fakeIssuer{creds: &storage.STSCredential{AccessKey: "ak-nil-rdb", ExpiresAt: time.Now().Add(time.Hour)}}
	svc := New(nil, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	c1, err := svc.Get(context.Background(), 1, 2, 1, "bucket-nil", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-nil"})
	require.NoError(t, err)
	assert.Equal(t, "ak-nil-rdb", c1.AccessKey)
	assert.Equal(t, 1, issuer.calls, "issuer must be called when Redis is unavailable")

	// Second call: cache is still disabled (nil rdb), so issuer fires again.
	c2, err := svc.Get(context.Background(), 1, 2, 1, "bucket-nil", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-nil"})
	require.NoError(t, err)
	assert.Equal(t, "ak-nil-rdb", c2.AccessKey)
	assert.Equal(t, 2, issuer.calls, "no caching without Redis; issuer called every time")
}

type blockingIssuer struct {
	releaseCh chan struct{}
	creds     *storage.STSCredential
	calls     int
	mu        sync.Mutex
}

func (b *blockingIssuer) Issue(_ context.Context, _ *storage.STSPolicy) (*storage.STSCredential, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	<-b.releaseCh // block until released
	return b.creds, nil
}

// callsCount returns the call count under the mutex so race-detector tests
// can read it safely from another goroutine.
func (b *blockingIssuer) callsCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestSTS_LoserDoesNotFetchDirectly verifies the singleflight integrity
// fix: when two concurrent Gets race and the winner's issuer call is slow
// (slower than the combined backoff window ~350ms), the loser MUST return an
// error rather than issuing its own issuer call. Without the fix the loser
// would fall through to fetchAndCache, causing 2 concurrent issuer calls.
//
// We never close releaseCh, so the winner stays in flight forever; the loser's
// backoff retries all miss and it must give up with an error. We assert:
//   - the loser gets a non-nil error (not a credential), and
//   - the issuer is called exactly once (the winner only).
func TestSTS_LoserDoesNotFetchDirectly(t *testing.T) {
	rdb := redisx.NewTestClient(t)

	// Issuer blocks forever (releaseCh never closed) → winner stays in flight
	// past the loser's backoff window.
	releaseCh := make(chan struct{})
	issuer := &blockingIssuer{
		releaseCh: releaseCh,
		creds:     &storage.STSCredential{AccessKey: "ak-slow", ExpiresAt: time.Now().Add(time.Hour)},
	}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	startCh := make(chan struct{})

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startCh
			_, err := svc.Get(context.Background(), 1, 2, 1, "bucket-slow", 30*time.Second, &storage.STSPolicy{Bucket: "bucket-slow"})
			errCh <- err
		}()
	}

	close(startCh) // fire both goroutines concurrently

	// The loser returns first (after ~350ms of backoff, while the winner is still
	// blocked in the issuer). Receive the first error — it must be the loser's.
	firstErr := <-errCh
	require.Error(t, firstErr, "loser must error, not fetch directly")
	assert.Contains(t, firstErr.Error(), "not yet cached after losing dedup race")

	// At this point the loser has given up but the winner is still blocked in the
	// issuer, so the issuer has been called exactly once. Assert singleflight
	// integrity before unblocking the winner.
	assert.Equal(t, 1, issuer.callsCount(), "issuer must be called exactly once; loser must not fetch directly")

	close(releaseCh) // let the winner finish so the test can drain
	secondErr := <-errCh
	require.NoError(t, secondErr, "winner must return the credential after unblock")

	wg.Wait()
}

// TestSTS_CacheTTLReservesSafetyMargin verifies the safety-margin fix on the
// write side: when the issuer returns a credential with a longer remaining
// lifetime than the resolved TTL minus the safety margin, the cache must
// expire stsSafetyMargin BEFORE the credential does — so any cache hit hands
// out a credential that still has at least stsSafetyMargin of usable life at
// the cloud provider.
//
// Without the fix, cacheTTL = min(resolvedTTL, credTTL), which means a cache
// hit at t = credTTL - ε returns a credential with ε seconds to live. OSS
// then rejects the in-flight upload with "SecurityToken expired".
//
// Here: cred lives 20m, resolvedTTL is 30m. cacheTTL must be ~15m (20m - 5m
// margin), NOT 20m.
func TestSTS_CacheTTLReservesSafetyMargin(t *testing.T) {
	rdb := redisx.NewTestClient(t)
	issuer := &fakeIssuer{creds: &storage.STSCredential{
		AccessKey: "ak-margin",
		ExpiresAt: time.Now().Add(20 * time.Minute),
	}}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 30 * time.Minute, MaxTTL: time.Hour})

	_, err := svc.Get(context.Background(), 1, 2, 1, "bucket-margin", 0, &storage.STSPolicy{Bucket: "bucket-margin"})
	require.NoError(t, err)

	key := cacheKey(1, 2, 1, "bucket-margin")
	ttl, err := rdb.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	// Expected ~15m (20m cred - 5m safety). Allow ±30s slack for test runtime.
	assert.InDelta(t, 15*time.Minute, ttl, float64(30*time.Second),
		"cache TTL must be cred lifetime minus stsSafetyMargin; got %v", ttl)
}

// TestSTS_NearExpiryCacheTreatedAsMiss verifies the read-side refresh window:
// a cached credential whose remaining lifetime is within stsReadRefreshWindow
// of expiry must be treated as a miss, forcing a fresh issuer call. This
// covers the edge case where fetchAndCache's clamp didn't shrink the cache
// TTL enough (e.g. credential lifetime was already shorter than the write
// margin), or where wall-clock advanced between write and read.
//
// We seed the cache directly with a near-expiry credential (15s left, inside
// the 30s refresh window) and verify that Get re-issues instead of returning
// the stale one.
func TestSTS_NearExpiryCacheTreatedAsMiss(t *testing.T) {
	rdb := redisx.NewTestClient(t)

	// Seed cache with a cred that's inside the read refresh window (15s < 30s).
	stale := &storage.STSCredential{
		AccessKey: "ak-stale",
		ExpiresAt: time.Now().Add(15 * time.Second),
	}
	key := cacheKey(1, 2, 1, "bucket-stale")
	raw, err := jsonx.Marshal(stale)
	require.NoError(t, err)
	require.NoError(t, rdb.Set(context.Background(), key, raw, 10*time.Minute).Err(),
		"seed cache directly with near-expiry cred; Redis key has plenty of TTL left")

	// Fresh issuer returns a long-lived cred.
	issuer := &fakeIssuer{creds: &storage.STSCredential{
		AccessKey: "ak-fresh",
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	svc := New(rdb, issuer, &config.STSConfig{DefaultTTL: 15 * time.Minute, MaxTTL: time.Hour})

	c, err := svc.Get(context.Background(), 1, 2, 1, "bucket-stale", 0, &storage.STSPolicy{Bucket: "bucket-stale"})
	require.NoError(t, err)
	assert.Equal(t, "ak-fresh", c.AccessKey,
		"near-expiry cached cred must be treated as miss; issuer must be called")
	assert.Equal(t, 1, issuer.calls, "issuer must be invoked once despite Redis key being present")
}

// TestSTS_ShortTTLWithMatchingMargin verifies users running with short
// MaxTTL get a correctly-sized cacheTTL when they also lower SafetyMargin
// accordingly. Config.Validate enforces SafetyMargin < MaxTTL, so this
// combination is the only valid configuration for short-TTL setups.
//
// Here: MaxTTL=3m, SafetyMargin=30s → cacheTTL for a 3m cred should be ~2m30s.
// Without the configurable margin (hardcoded 5m), this would either go
// negative and skip the clamp entirely, or Config.Validate would reject the
// config — depending on which fix landed first.
func TestSTS_ShortTTLWithMatchingMargin(t *testing.T) {
	rdb := redisx.NewTestClient(t)
	issuer := &fakeIssuer{creds: &storage.STSCredential{
		AccessKey: "ak-short",
		ExpiresAt: time.Now().Add(3 * time.Minute),
	}}
	svc := New(rdb, issuer, &config.STSConfig{
		DefaultTTL:        3 * time.Minute,
		MaxTTL:            3 * time.Minute,
		SafetyMargin:      30 * time.Second,
		ReadRefreshWindow: 5 * time.Second,
	})

	c, err := svc.Get(context.Background(), 1, 2, 1, "bucket-short", 0, &storage.STSPolicy{Bucket: "bucket-short"})
	require.NoError(t, err)
	assert.Equal(t, "ak-short", c.AccessKey)

	key := cacheKey(1, 2, 1, "bucket-short")
	ttl, err := rdb.TTL(context.Background(), key).Result()
	require.NoError(t, err)
	// Expected ~2m30s (3m cred - 30s margin). Allow ±10s slack for runtime.
	assert.InDelta(t, 150*time.Second, ttl, float64(10*time.Second),
		"cacheTTL must equal credTTL - cfg.SafetyMargin; got %v", ttl)
	assert.Positive(t, int64(ttl), "cacheTTL must never go negative")
}
