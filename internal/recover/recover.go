// SPDX-License-Identifier: MIT

// Package recover implements `topctl recover` — TOP's independent
// recovery path (ADR-0005). It runs ON the host and depends on exactly
// three things: the Dokku CLI, the filesystem, and the SQLite state file
// (`<dataDir>/top.db`). It never depends on — and there is no code path
// here that talks to — the controller app, Prometheus, or Grafana being
// healthy; when the controller is down, diagnose must still produce a
// complete read-only report, and restart/redeploy must still be runnable
// with their exact manual command equivalents printed (the
// backup-recovery contract: "recovery output doubles as a runbook").
//
// Milestone 2 slice (T-2xx slice of docs/contracts/backup-recovery.md
// "Recovery"):
//
//   - diagnose    — fully real, read-only, doctor-shaped findings.
//   - restart     — real, minimal: `dokku ps:restart <app>` + a bounded
//     ps:report health poll (ADR-0009 `--confirm` gated at the CLI).
//   - redeploy    — real, minimal: pinned bootstrap image by default,
//     or the last-known-good revision from the store's checkpoint
//     (`git:sync` + `ps:rebuild`). M5 hardens the edge cases (T-503).
//   - restore     — SCAFFOLD ONLY this wave: refuses to run until the
//     ADR-0007 safeguard order exists (Milestone 5). The safeguards come
//     first because restore restores *over existing metadata*; the
//     contract forbids a naive restore implementation.
//
// Import hygiene: this package imports internal/doctor (for the Finding/
// Report shape — recovering and preflighting print the same kind of
// report, so agents read one shape), internal/export (the only process
// seam), internal/config (config validity check), and
// internal/store/sqlite (the state file). It does not import internal/cli
// (the arrow goes the other way), and it does not import
// internal/backend/dokku — the recovery path parses the few report keys
// it needs locally so it never depends on the full backend stack being
// loadable, and stays the deliberately-small independent path ADR-0005
// describes.
package recover

import (
	"log/slog"
	"time"

	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/exec"
)

// Reuse of internal/doctor's finding shape: recover prints findings in
// the doctor vocabulary exactly (same fields, same severities) — see
// internal/doctor/doctor.go for the style contract.
type (
	// Finding is internal/doctor.Finding — re-declared locally (a type
	// alias keeps them interchangeable and greps findable).
	Finding = doctor.Finding
	// Report is internal/doctor.Report: findings + OK()/Count(). Alias
	// rather than a new struct: one report shape across topctl.
	Report = doctor.Report
	// Severity is internal/doctor.Severity (ok/info/warn/fail).
	Severity = doctor.Severity
)

// The severity constants are re-bound here so recover file bodies read
// symmetric with doctor's (fail/warn/info/ok, same semantics and the
// same exit-code agreement: report fails → CLI exit 1).
const (
	sevOK   = doctor.SeverityOK
	sevInfo = doctor.SeverityInfo
	sevWarn = doctor.SeverityWarn
	sevFail = doctor.SeverityFail
)

// Defaults for the bounded health wait (docs/contracts/backup-recovery.md
// restart line is bounded "3-minute cap" — the contract's fixed budget).
const (
	DefaultHealthTimeout = 3 * time.Minute
	DefaultPollInterval  = 2 * time.Second
)

// DefaultBootstrapImage is the pinned controller image used when no
// revision can be resolved from the store (ADR-0005 "bootstrap-pinned
// image"). PLACEHOLDER pending the installer's T-2xx pin decision: the
// value is deliberately fictional-but-formatted like the rest of the
// committed examples (no real registry); M5 hardening (T-503) replaces
// it with the ldflags-pinned tag that install itself used.
const DefaultBootstrapImage = "ghcr.io/example/top/topctl:bootstrap-pinned"

// dbFile is the state store's file name inside dataDir (the docs'
// `/var/lib/top/top.db`; wiring note: no path knob exists yet — M5 makes
// it follow the store's configured path one source of truth).
const dbFile = "top.db"

// configFile is TOP's own config filename as the docs spell it
// (internal/config: "top-config.yaml on the controller host").
const configFile = "top-config.yaml"

// Options parameterize the recovery operations; zero values select the
// production seams (real exec runner, real statfs disk probe, system
// clock). Tests inject fakes per the same discipline as internal/doctor.
type Options struct {
	// Runner runs the Dokku CLI (the ONLY host dependency, ADR-0005).
	// nil → exec.NewFastExec().
	Runner exec.Runner
	// DataDir is TOP's state root (internal/config default /var/lib/top).
	DataDir string
	// GitURL is the platform repo remote (config.Git.URL); redeploy's
	// revision path syncs from here.
	GitURL string
	// GitBranch is the platform-repo branch (config.Git.Branch).
	GitBranch string
	// ConfigFile is the config file diagnose validates; "" means
	// DataDir/top-config.yaml. The CLI passes the operator's --config
	// path when one was given.
	ConfigFile string
	// BackupsDir is the backup tree (ADR-0007's /var/lib/top/backups):
	// restore writes its safety snapshots into BackupsDir/safety and
	// stages offsite-pulled backups into BackupsDir/remote-staging.
	// "" → DataDir/backups.
	BackupsDir string
	// BootstrapImage is the pinned controller image for the
	// no-store-revision fallback; "" → DefaultBootstrapImage.
	BootstrapImage string
	// DiskProbe is the free-disk read (internal/doctor's DiskProbe type,
	// reused so both tools sweep the same seam). nil → statfs on the
	// dataDir's nearest existing ancestor.
	DiskProbe doctor.DiskProbe
	// Now yields the clock for staleness math. nil → time.Now.
	Now func() time.Time
	// HealthTimeout bounds the restart/redeploy wait-for-health loop;
	// 0 → DefaultHealthTimeout (3m, the contract's restart budget).
	HealthTimeout time.Duration
	// PollInterval spaces ps:report polls during the health wait;
	// 0 → DefaultPollInterval.
	PollInterval time.Duration
	// Log receives the restore path's engine log lines (slog); optional
	// (nil → discard).
	Log *slog.Logger
}

// applyDefaults fills the production seams (mutating only the Options
// value — the identical discipline to doctor's applyDefaults).
func (o *Options) applyDefaults() {
	if o.Runner == nil {
		o.Runner = exec.NewFastExec()
	}
	if o.DataDir == "" {
		o.DataDir = doctor.DefaultDataDir
	}
	if o.ConfigFile == "" {
		o.ConfigFile = o.DataDir + "/" + configFile
	}
	if o.BackupsDir == "" {
		o.BackupsDir = o.DataDir + "/backups"
	}
	if o.BootstrapImage == "" {
		o.BootstrapImage = DefaultBootstrapImage
	}
	if o.DiskProbe == nil {
		o.DiskProbe = systemDiskProbe
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = DefaultHealthTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
}

// dbPath is the state file this build understands for the recovery
// contract: <dataDir>/top.db.
func dbPath(opts Options) string { return opts.DataDir + "/" + dbFile }

// logOrDiscard gives restore's engine a usable logger when none was
// injected (the recover CLI carries no logging flag of its own).
func (o *Options) logOrDiscard() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.New(slog.DiscardHandler)
}

// controllerApp resolves the recovery contract's fixed app name. The
// controller namespace is `core` by ADR-0004/0005 (reserved and
// immutable in every legal config), so the recovery path — which must
// not depend on config parsing succeeding — pins the ADR name directly
// and derives nothing from system opinion.
func controllerApp() string { return doctor.ControllerAppName }

// Result is the record of one real recovery action: what was decided,
// what ran (the printed output doubles as a runbook per the
// backup-recovery contract), and what was observed.
type Result struct {
	// App is the Dokku app the action targeted.
	App string
	// Revision is the resolved revision for redeploy ("" → image path).
	Revision string
	// Image is the image used (redeploy image path only).
	Image string
	// Source names where the decision came from ("--revision flag",
	// "last-known-good checkpoint", "bootstrap-pinned image").
	Source string
	// Commands is the manual command equivalents, in execution order.
	Commands []string
	// Notes carry extra context (e.g. log tails or fallback reasons)
	// that go to humans but are not commands.
	Notes []string
}

// dataMap renders a successful action into the envelope Data payload.
func (r *Result) DataMap() map[string]any {
	m := map[string]any{"app": r.App, "commands": r.Commands}
	if r.Revision != "" {
		m["revision"] = r.Revision
	}
	if r.Image != "" {
		m["image"] = r.Image
	}
	if r.Source != "" {
		m["source"] = r.Source
	}
	if len(r.Notes) != 0 {
		m["notes"] = r.Notes
	}
	return m
}
