// Package jobs owns the cron scheduler for storage-service periodic
// background work. The Scheduler type is a pure cron wrapper: it knows how to
// build (or accept) a cron.Cron, expose AddFunc for callers to register jobs,
// and adapt to lifecycle.Service so it integrates with the parent service's
// lifecycle.Manager. It does NOT know which domain jobs to run — the parent
// service decides that and calls AddFunc.
package jobs

import (
	"fmt"

	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/robfig/cron/v3"
)

// Scheduler wraps a cron.Cron and adapts it to lifecycle.Service. Callers
// register periodic work via AddFunc; Start launches the scheduler, Stop
// blocks until in-flight jobs drain — but ONLY when Scheduler owns the cron
// (built it itself). When a cron is injected via Deps.Cron, the caller owns
// its lifecycle and Start/Stop are no-ops; robfig/cron's Stop() is not
// idempotent (each call spawns a goroutine waiting on jobWaiter), so a
// borrowed cron must be lifecycle-managed by exactly one owner.
type Scheduler struct {
	cron     *cron.Cron
	ownsCron bool
}

// Deps injects the cronx.Config used to build a new cron, and an optional
// pre-built cron. If Cron is non-nil, Config is ignored and the caller
// retains lifecycle ownership (Start/Stop become no-ops on the Scheduler).
type Deps struct {
	Config *cronx.Config
	Cron   *cron.Cron
}

// Compile-time assertion that *Scheduler satisfies lifecycle.Service.
var _ lifecycle.Service = (*Scheduler)(nil)

// New returns a Scheduler wrapping either the injected cron or a freshly
// built one. At least one of Deps.Config or Deps.Cron must be non-nil.
func New(d *Deps) (*Scheduler, error) {
	c := d.Cron
	owns := false
	if c == nil {
		if d.Config == nil {
			return nil, fmt.Errorf("jobs: Deps.Config required when Deps.Cron is nil")
		}
		var err error
		c, err = cronx.New(d.Config)
		if err != nil {
			return nil, fmt.Errorf("jobs: init cron: %w", err)
		}
		owns = true
	}
	return &Scheduler{cron: c, ownsCron: owns}, nil
}

// AddFunc registers a periodic job on the scheduler. Caller picks the spec
// and the function; Scheduler does not interpret either.
func (s *Scheduler) AddFunc(spec string, cmd func()) error {
	if _, err := s.cron.AddFunc(spec, cmd); err != nil {
		return fmt.Errorf("jobs: add func: %w", err)
	}
	return nil
}

// Start launches the cron scheduler. No-op when the cron was injected via
// Deps.Cron (caller owns lifecycle in that case). robfig/cron's Start is
// idempotent so multiple calls on an owned cron are safe, but we still skip
// when borrowing to respect the single-owner contract.
func (s *Scheduler) Start() error {
	if s.ownsCron {
		s.cron.Start()
	}
	return nil
}

// Stop signals the scheduler to halt and blocks until all in-flight jobs
// finish. No-op when the cron was injected via Deps.Cron (caller owns
// lifecycle). robfig/cron's Stop is NOT idempotent — each call spawns a
// goroutine waiting on jobWaiter — so the borrower must never call it; only
// the owner (this Scheduler when ownsCron=true) may.
func (s *Scheduler) Stop() error {
	if s.ownsCron {
		<-s.cron.Stop().Done()
	}
	return nil
}
