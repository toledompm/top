// SPDX-License-Identifier: MIT

// Package doctor implements `topctl doctor` — the read-only preflight
// suite (installer contract Phase 0, docs/contracts/installer.md; ADR-0009).
//
// Doctor NEVER mutates the host: it only reads process-adjacent state
// (uname/os-release, /proc/meminfo, statfs), does one bind-close probe on
// ports 80/443 (optional, --skip-ports opt-out), and runs a handful of
// version/health commands through internal/exec (docker, dokku, sudo,
// sysctl on macOS). Every non-ok finding carries a Hint; TOP prints exact
// fixes, never escalates, and gate-worthy changes (installing Dokku) belong
// to `topctl install --install-dokku`, not here.
//
// Import hygiene: this package deliberately does NOT import internal/cli
// (internal/cli imports doctor to wire the command body). Its render
// helpers mirror the cli envelope style in spirit and document the tiny
// shared shape (schemaVersion: 1, command: "doctor") instead of sharing
// code — see render.go.
package doctor

import (
	"context"
	"runtime"

	"github.com/toledompm/top/internal/exec"
)

// Domain-owned floors and defaults (contract constants live here, not in
// internal/cli, so doctor has no upward import). MinDokku mirrors
// internal/cli.MinDokkuVersion and ADR-0001 ("minimum supported Dokku:
// 0.38.x"); keep both in sync.
const (
	// MinDokkuVersion is the oldest Dokku release TOP supports (ADR-0001).
	MinDokkuVersion = "0.38"
	// MinDockerVersion is the oldest Docker engine release TOP accepts.
	// Recorded assumption (maintained in README.md): no platform doc
	// pins a Docker floor yet; 24.0 was the prevailing stable engine
	// line at planning time.
	MinDockerVersion = "24.0"

	// ControllerAppName is the reserved Dokku app name for TOP's own
	// controller (ADR-0009 naming layer).
	ControllerAppName = "core-top-controller"

	// DefaultDataDir is TOP's state root (internal/config default).
	DefaultDataDir = "/var/lib/top"
)

// Severity is a finding's weight. fail → exit 1; warn/info/ok are
// informational and never change the exit code (documented in README).
type Severity string

// The severities, from benign to blocking. SeverityInfo is for states
// that are fine (e.g. Dokku absent — a guided install will supply it)
// but that the operator should see.
const (
	SeverityOK   Severity = "ok"
	SeverityInfo Severity = "info"
	SeverityWarn Severity = "warn"
	SeverityFail Severity = "fail"
)

// Finding is one check's verdict. IDs are stable machine strings
// ("docker/daemon"); agents read ID+Severity, humans read Message+Hint.
type Finding struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Check    string   `json:"check"`
	Message  string   `json:"message"`
	// Hint is the actionable fix guidance; never empty for a non-ok
	// finding (ADR-0009: doctor verifies and prints fixes).
	Hint string `json:"hint,omitempty"`
}

// Report is the full preflight result. It is a value type: checks append
// findings and Run returns the assembled report.
type Report struct {
	Findings []Finding
}

// OK reports whether the report is install-worthy: no fail findings.
// Warnings do not foil it (see README — the exit-code contract).
func (r Report) OK() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityFail {
			return false
		}
	}
	return true
}

// Count returns how many findings have the given severity.
func (r Report) Count(sev Severity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Sev() == sev {
			n++
		}
	}
	return n
}

// Sev guards against a zero-value Finding ("" severity) reading as the
// unknown "" severity; internal use only.
func (f Finding) Sev() Severity {
	if f.Severity == "" {
		return SeverityInfo
	}
	return f.Severity
}

// ListenProbe abstracts the port probe ("can I bind and immediately
// close?") so tests never need privileged sport. network/addr mirror
// net.Listen's signature.
type ListenProbe func(network, addr string) (closeFn func(), err error)

// MemProbe abstracts the total-RAM read for tests.
type MemProbe func() (totalBytes uint64, err error)

// DiskProbe abstracts the free-bytes read for tests.
type DiskProbe func(path string) (freeBytes uint64, err error)

// Orchestrator abstracts OS identity so tests can fake GOOS. It returns
// (goos like "linux"/"darwin", arch like "amd64"/"arm64").
type OSProbe func() (goos, arch string)

// Options parameterizes Run; zero values select the real host seams.
type Options struct {
	// Runner executes version/docker/dokku/sudo/sysctl probes (ADR-0006
	// layer). nil → exec.NewFastExec().
	Runner exec.Runner
	// OSProbe answers GOOS/GOARCH. nil → runtime.GOOS/GOARCH.
	OSProbe OSProbe
	// Probe is the port listen-probe. nil → liveListener. Tests inject
	// a fake; nobody binds :80+ in a test.
	Probe ListenProbe
	// MemProbe / DiskProbe override the RAM and free-disk reads
	// (nil → system readers via probeMem / probeDisk).
	MemProbe  MemProbe
	DiskProbe DiskProbe
	// DataDir is TOP's state root (from internal/config; "" → DefaultDataDir).
	DataDir string
	// ReservedNamespaces are the reserved app-name prefixes (ADR-0009);
	// nil → config.Default namespaces ["core","observability"].
	ReservedNamespaces []string
	// SkipPorts opts out of the 80/443 probe (operator choice; the
	// bind-close window is a brief TOCTOU race — see README).
	SkipPorts bool
	// OSReleasePath is where checkDistro reads identity. Tests point it
	// at a fake file; "" → /etc/os-release (linux only).
	OSReleasePath string
}

// Run executes every check in the Phase 0 list and assembles the report.
// It never fails as a call: every per-check problem is a Finding; the
// returned report's OK() decides the exit code. Run does all checks even
// when some fail, so one run shows the operator every fix at once.
func Run(ctx context.Context, opts Options) Report {
	opts.applyDefaults()
	r := Report{}

	r.checkOS(opts)
	r.checkDocker(ctx, opts.Runner)
	apps := r.checkDokku(ctx, opts)
	r.checkPorts(opts)
	r.checkResources(opts)
	r.checkSudo(ctx, opts.Runner)
	r.checkNames(apps, opts)
	r.checkDataDir(opts)
	return r
}

func (o *Options) applyDefaults() {
	if o.Runner == nil {
		o.Runner = exec.NewFastExec()
	}
	if o.OSProbe == nil {
		o.OSProbe = func() (string, string) { return runtime.GOOS, runtime.GOARCH }
	}
	if o.Probe == nil {
		o.Probe = liveListener
	}
	if o.MemProbe == nil {
		o.MemProbe = systemMemProbe(o.Runner)
	}
	if o.DiskProbe == nil {
		o.DiskProbe = systemDiskProbe
	}
	if o.DataDir == "" {
		o.DataDir = DefaultDataDir
	}
	if o.ReservedNamespaces == nil {
		o.ReservedNamespaces = []string{"core", "observability"}
	}
}
