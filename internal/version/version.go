// Package version holds build-time version info injected via -ldflags (see
// the Makefile build target), plus runtime-only values read at startup.
//
// Defaults ("dev" / "unknown") apply when built without ldflags (e.g. `go
// run` or tests). When built from a git repo with Go 1.18+, the toolchain
// also embeds VCS info (vcs.revision / vcs.time / vcs.modified) accessible
// via runtime/debug.ReadBuildInfo(), used here as a fallback for the commit
// hash so even `go run` reports something useful.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build-time injected via `go build -ldflags "-X ..."` (see Makefile).
var (
	Version   = "dev"     // semantic version, e.g. "v1.2.3" (git describe --tags)
	GitCommit = "unknown" // short commit hash (git rev-parse --short HEAD)
	GitBranch = "unknown" // git branch (git rev-parse --abbrev-ref HEAD)
	BuildTime = "unknown" // RFC3339 UTC build timestamp (date -u +%Y-%m-%dT%H:%M:%SZ)
)

// Info aggregates version info for Pong / startup logging.
type Info struct {
	Version   string // semantic version (ldflags)
	GitCommit string // short commit hash (ldflags, or VCS-embedded fallback)
	GitBranch string // git branch (ldflags)
	BuildTime string // build timestamp, RFC3339 UTC (ldflags)
	GoVersion string // runtime.Version() — read at runtime, not injected
}

// Get returns the build info. Prefers ldflags-injected values; falls back to
// the VCS-embedded revision (Go 1.18+ -buildvcs) for the commit hash when
// ldflags were not supplied (e.g. `go run`).
func Get() Info {
	i := Info{
		Version:   Version,
		GitCommit: GitCommit,
		GitBranch: GitBranch,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if i.GitCommit == "unknown" {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					i.GitCommit = s.Value[:7]
				}
			}
		}
	}
	return i
}

// String returns a one-line summary for startup logs.
func (i Info) String() string {
	return fmt.Sprintf("%s (commit=%s branch=%s built=%s %s)",
		i.Version, i.GitCommit, i.GitBranch, i.BuildTime, i.GoVersion)
}
