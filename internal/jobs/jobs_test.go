package jobs

import (
	"testing"

	"github.com/servekit/go-common/cronx"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalCronConfig returns a cronx.Config just sufficient for jobs.New to
// build a cron. Timezone empty is fine — cronx defaults to UTC.
func minimalCronConfig() *cronx.Config {
	return &cronx.Config{OverlapPolicy: "skip"}
}

// TestNew_DefaultBuildsCron verifies that when Deps.Cron is nil, New builds
// its own cron instance (non-nil).
func TestNew_DefaultBuildsCron(t *testing.T) {
	s, err := New(&Deps{Config: minimalCronConfig()})
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.NotNil(t, s.cron, "Scheduler should build its own cron when none injected")
	assert.Empty(t, s.cron.Entries(), "no jobs should be registered yet")
}

// TestNew_WithCronUsesInjected verifies that an injected cron is used as-is.
// Deps.Config is ignored in this case.
func TestNew_WithCronUsesInjected(t *testing.T) {
	injected := cron.New()
	s, err := New(&Deps{Cron: injected})
	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Same(t, injected, s.cron, "Scheduler should use the injected cron instance")
	assert.False(t, s.ownsCron, "ownsCron must be false when cron is injected")
}

// TestNew_NilConfigAndCronErrors verifies that New rejects Deps with neither
// Config nor Cron set — at least one is required to know what to wrap.
func TestNew_NilConfigAndCronErrors(t *testing.T) {
	_, err := New(&Deps{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Deps.Config required")
}

// TestNew_OwnsCronWhenBuilt verifies ownsCron is true when Scheduler builds
// its own cron. This is the precondition for Start/Stop actually touching
// the underlying cron — robfig/cron's Stop is not idempotent, so a borrowed
// cron must never be stopped by the borrower.
func TestNew_OwnsCronWhenBuilt(t *testing.T) {
	s, err := New(&Deps{Config: minimalCronConfig()})
	require.NoError(t, err)
	assert.True(t, s.ownsCron, "ownsCron must be true when Scheduler builds the cron")
}

// TestStartStop_NoOpWhenInjected verifies that when a cron is injected,
// Start and Stop do NOT touch the underlying cron. Caller owns lifecycle.
// We verify by checking that ownsCron=false makes Start/Stop into no-ops;
// calling s.cron.Stop() directly on an un-started injected cron would be
// safe per robfig/cron v3.0.1 source (running=false short-circuits), but
// the contract is "borrower never calls Stop" — so we just don't.
func TestStartStop_NoOpWhenInjected(t *testing.T) {
	injected := cron.New() // not started
	s, err := New(&Deps{Cron: injected})
	require.NoError(t, err)
	require.False(t, s.ownsCron)

	// Both should be no-ops; no panic, no error, no observable state change.
	require.NoError(t, s.Start())
	require.NoError(t, s.Stop())
}

// TestAddFunc_RegistersJob verifies AddFunc attaches a job to the scheduler.
func TestAddFunc_RegistersJob(t *testing.T) {
	s, err := New(&Deps{Config: minimalCronConfig()})
	require.NoError(t, err)

	require.NoError(t, s.AddFunc("*/5 * * * *", func() {}))
	assert.Len(t, s.cron.Entries(), 1, "one job should be registered")
}

// TestAddFunc_InvalidSpecReturnsError verifies that a malformed spec makes
// AddFunc fail with a non-nil error.
func TestAddFunc_InvalidSpecReturnsError(t *testing.T) {
	s, err := New(&Deps{Config: minimalCronConfig()})
	require.NoError(t, err)

	err = s.AddFunc("not-a-valid-cron-spec", func() {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add func")
	assert.Empty(t, s.cron.Entries(), "no job should be registered on error")
}

// TestStartStop_NonBlocking verifies Start returns immediately and Stop
// returns promptly when no jobs are in-flight.
func TestStartStop_NonBlocking(t *testing.T) {
	s, err := New(&Deps{Config: minimalCronConfig()})
	require.NoError(t, err)

	require.NoError(t, s.Start())
	require.NoError(t, s.Stop())
}
