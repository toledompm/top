// SPDX-License-Identifier: MIT

// Package install implements `topctl install` and `topctl uninstall` —
// the phased installer and ownership-safe teardown of
// docs/contracts/installer.md (ADR-0005 bootstrap, ADR-0009 ownership).
//
// Shape:
//
//	Phase 0  preflight      — internal/doctor.Run plus installer-specific
//	                          checks (repo reachable/ non-empty, deploy/key
//	                          paths). Read-only; never installs anything.
//	Phase 1  inventory      — internal/inventory.Snapshot +
//	                          CreateLedgerFromSnapshot (created_by anchored
//	                          on this phase's install-operation record).
//	Phase 2  dokku setup    — verify version/health; with --install-dokku and
//	                          dokku absent, the pinned upstream bootstrap
//	                          script, sudo via printed-exact-command fallback
//	                          (never passwordless-assumed).
//	Phase 3  wiring         — dataDir/{,secrets,git,backups} (0700 dirs,
//	                          0600 files), deploy-key/known_hosts provisioning,
//	                          shared internal network via the backend.
//	Phase 4  bootstrap deploy — storage entry + app + pinned-image deploy,
//	                          bounded health wait; every created asset is a
//	                          bootstrap-owned ledger row.
//	Phase 5  report         — created/adopted/left-alone/checked lists and
//	                          the next suggested commands.
//
// Phase state lives in the store's operations records (kind "install",
// details JSON {"phase": N, ...}); re-running detects completed phases and
// resumes. DryRun prints the full plan and performs ZERO mutations (no
// store writes, no argv beyond read-only probes, no filesystem changes).
//
// Import hygiene (ADR-0006): this package imports internal/doctor,
// internal/inventory, internal/exec, internal/config, internal/secret,
// internal/domain, internal/store (interface only — sqlite.Open is the
// one wiring point, mirroring internal/recover). It does NOT import
// internal/backend/dokku or internal/cli: the CLI (composition root) wires
// the concrete backend via Options.Backend and renders results.
//
// The first-reconciliation handoff (contract phase 5's adopt-and-reconcile,
// "Readiness(core)" gate) is Milestone 3/4 work: this package records the
// bootstrap deployment and stops; the deployment stays bootstrap-owned
// until the controller adopts itself (store.MarkManaged), which this
// package deliberately does NOT do (adoption RECORD only per the M2 scope).
package install

import (
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/store"
)

// Shared constants. The controller's ref is derived through ONE author
// (domain.AppName on the core namespace — ADR-0004/0009); the data dir,
// Dokku floor, and the doctor's controller app name keep their single
// authors (internal/config, ADR-0001, doctor.ControllerAppName).
const (
	// StorageEntryName is the named Dokku storage entry carrying the
	// controller's SQLite state (ADR-0005: SQLite on a Dokku persistent
	// storage mount; backup-recovery contract: <dataDir>/top.db).
	StorageEntryName = "top-metadata"

	// InternalNetwork is the shared Docker network for routing.mode
	// internal (platform-backend contract; EnsureNetwork).
	InternalNetwork = "top-internal"

	// DefaultBranch is the platform-repo branch when none configured.
	DefaultBranch = "main"
)

// DataDirDefault mirrors doctor.DefaultDataDir (single author: doctor's
// constant is the docs' "/var/lib/top"; this is a read-only alias).
var DataDirDefault = doctor.DefaultDataDir

// DefaultControllerImage / DefaultControllerTag pin the bootstrap deploy
// (ADR-0005 "bootstrap pins a specific version"). The pair composes to the
// same fictional-but-formatted reference internal/recover's
// DefaultBootstrapImage spells out ("ghcr.io/example/top/topctl:
// bootstrap-pinned"; no real registry — committed examples are fictional,
// ADR-0014-style). Bumping either is a conscious release decision: the
// recovery path's fallback and the next install must agree.
var (
	DefaultControllerImage = "ghcr.io/example/top/topctl"
	DefaultControllerTag   = "bootstrap-pinned"
)

// Typed failure classes. The CLI maps them onto docs/contracts/cli.md exit
// codes; the values are message text, the identity is classification.
var (
	// ErrPreflightFailed: doctor (or the installer's own preflight
	// checks) produced fail-severity findings — abort before anything
	// mutates (CLI exit 1, the drift/diagnose space: recoverable).
	ErrPreflightFailed = errors.New("install: preflight failed")
	// ErrGateRequired: a mutating path ran without the ADR-0009 approval
	// marker. The CLI's --confirm prompt produces the marker; calling
	// Install/Uninstall directly must pass Confirm.
	ErrGateRequired = errors.New("install: approval gate not satisfied (--confirm)")
	// ErrSudoRefused: the sudo step could not proceed non-interactively
	// and TOP printed the exact command the operator can run themselves
	// (ADR-0009: print, never assume passwordless, never escalate).
	ErrSudoRefused = errors.New("install: sudo refused non-interactive run")
	// ErrDokkuRequired: Dokku is absent and --install-dokku was not
	// passed — a clean stop with instructions (installer contract).
	ErrDokkuRequired = errors.New("install: dokku absent and --install-dokku not passed")
	// ErrRepoMissing: the platform repo URL is missing, empty, or
	// unreachable — per ADR-0005's no-init rule a clean preflight error
	// with instructions (--init is T-212/M3 work; nothing auto-creates).
	ErrRepoMissing = errors.New("install: platform repo missing, empty, or unreadable")
	// ErrStoreUnavailable: the state store could not be opened.
	ErrStoreUnavailable = errors.New("install: state store unavailable")
	// ErrNothingInstalled: uninstall found no recorded TOP state at the
	// data dir — refusing to tear down something TOP cannot prove owns
	// is the point of ADR-0009.
	ErrNothingInstalled = errors.New("uninstall: no TOP state found")
	// ErrLeaseHeld: the serialization lease is held (controller running);
	// uninstall stops rather than racing it.
	ErrLeaseHeld = errors.New("uninstall: controller lease is held")
)

// Options parameterize Install. Zero values select production seams; every
// test-doubles ability mirrors internal/doctor's and internal/recover's
// Options discipline (Runner / probes / clock injectable, defaults inside).
type Options struct {
	// Runner runs every external process (dokku probes, git ls-remote,
	// wget, sudo, ssh-keygen surprises). nil → exec.NewFastExec().
	Runner exec.Runner

	// Backend is the PlatformBackend (the CLI wires internal/backend/dokku
	// — the composition root, mirroring how doctor/recover get theirs).
	// REQUIRED once not DryRun; phases 3-5 and uninstall refuse otherwise.
	Backend backend.PlatformBackend

	// Store is the state store. nil → sqlite at <dataDir>/top.db, opened
	// and closed BY Install/Uninstall (the composition's default seam).
	// Tests inject a store (":memory:" or a temp file) and manage its life.
	Store store.Store

	// RepoURL is the platform git repository (CLI positional arg; must
	// already exist WITH content — ADR-0005's no-init rule for now).
	RepoURL string
	// Branch is the platform-repo branch ("" → DefaultBranch).
	Branch string

	// InstallDokku opts into the approval-gated Dokku bootstrap (phase 2).
	InstallDokku bool
	// Confirm is the deterministic ADR-0009 approval marker. The CLI's
	// requireConfirm produces it (interactive TTY prompt or --confirm);
	// Install/Uninstall refuse to mutate without it. DryRun needs no
	// approval (nothing mutates).
	Confirm bool
	// DryRun prints the full plan and performs zero mutations.
	DryRun bool

	// DataDir is TOP's state root ("" → /var/lib/top).
	DataDir string
	// DeployKey is the operator-supplied private deploy-key FILE path
	// (provisioning path (a): TOP copies it into <dataDir>/git/ 0600).
	// Path-generation (ssh-keygen subprocess) is deliberately deferred
	// (M3; TODO note in phase 3) — v0 requires this path and refuses
	// otherwise, with an actionable finding.
	DeployKey string
	// KnownHosts is the operator-supplied pinned known_hosts FILE path
	// (copied into <dataDir>/git/ 0600). Required alongside DeployKey in
	// v0 — an unpinned known_hosts would pin nothing.
	KnownHosts string

	// ControllerImage/Tag override the pinned bootstrap image (defaults
	// above; draft tests use fictional values).
	ControllerImage string
	ControllerTag   string

	// Namespaces are the reserved namespace prefixes (nil → config's
	// immutables).
	Namespaces []string
	// SkipPorts toggles doctor's 80/443 probe.
	SkipPorts bool

	// Pre-seams for tier-1 tests (injected ONLY by tests in practice).
	OSProbe       doctor.OSProbe
	Probe         doctor.ListenProbe
	MemProbe      doctor.MemProbe
	DiskProbe     doctor.DiskProbe
	OSReleasePath string
	// Now is the clock (op records, ledger timestamps). nil → UTC now.
	Now func() time.Time
	// DeployTimeout bounds the bootstrap image deploy (0 → 10 min, the
	// contract's budget; the adapter enforces its own identical cap).
	DeployTimeout time.Duration
	// HealthTimeout bounds the post-deploy health wait (0 → 3 min,
	// matching recover's restart budget).
	HealthTimeout time.Duration
	// PollInterval spaces the health-wait Inspect polls (0 → 2 s).
	PollInterval time.Duration
}

// ImageRef renders the pinned controller image reference.
func (o Options) ImageRef() string {
	return o.controllerImage() + ":" + o.controllerTag()
}

func (o Options) controllerImage() string {
	if o.ControllerImage != "" {
		return o.ControllerImage
	}
	return DefaultControllerImage
}

func (o Options) controllerTag() string {
	if o.ControllerTag != "" {
		return o.ControllerTag
	}
	return DefaultControllerTag
}

func (o *Options) applyDefaults() {
	if o.Runner == nil {
		o.Runner = exec.NewFastExec()
	}
	if o.DataDir == "" {
		o.DataDir = DataDirDefault
	}
	if o.Branch == "" {
		o.Branch = DefaultBranch
	}
	if len(o.Namespaces) == 0 {
		o.Namespaces = []string{"core", "observability"}
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.DeployTimeout <= 0 {
		o.DeployTimeout = 10 * time.Minute
	}
	if o.HealthTimeout <= 0 {
		o.HealthTimeout = 3 * time.Minute
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
		// Install-local env knob (documented in README): quickens the
		// health-wait loop for hermetic tests and CI. Not a TOP-config
		// field — it is an install-phase pacing control.
		if v := os.Getenv("TOP_INSTALL_POLL_INTERVAL_MS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				o.PollInterval = time.Duration(n) * time.Millisecond
			}
		}
	}
}

// ControllerRef resolves the controller's qualified ref through the one
// derivation rule (domain parse/validate); phase 4 and uninstall agree
// via this, never via bare app-name literals.
func ControllerRef() domain.ServiceRef {
	return domain.Ref(domain.CoreNamespace, domain.Name("top-controller"))
}

// ControllerAppName derives the Dokku app name (domain.AppName) of
// ControllerRef — "core-top-controller" by the ADR-0009 naming layer.
// domain.CoreNamespace is immutable-reserved in every legal config, so
// this cannot fail for the platform's own refs; the fallback is doctor's
// same-name constant purely as belt.
func (o Options) ControllerAppName() string {
	app, err := domain.AppName(ControllerRef().Namespace, ControllerRef().Name)
	if err != nil {
		return doctor.ControllerAppName
	}
	return app
}

// Result is the phase-5 report: what happened, in the contract's lists.
// It is plain data — the CLI nests it in the envelope's Data and the text
// renderer prints it; a Result returned alongside an error carries the
// partial progress (contract: "leave prior phases untouched").
type Result struct {
	// Resumed is true when at least one phase was skipped because the
	// store already recorded its completion.
	Resumed bool
	// PhasesRun (in order) are the phase names this invocation executed.
	PhasesRun []string
	// PhasesSkipped are the phases skipped as already-complete.
	PhasesSkipped []string
	// Created lists resources/dirs/format decisions TOP created this run
	// ("app core-top-controller", "network top-internal", "dir ...").
	Created []string
	// Adopted lists pre-existing TOP-shaped resources EnsureApp took
	// over (bootstrap-resume path — e.g. re-run after a failed deploy).
	Adopted []string
	// LeftAlone lists the pre-existing resources the ownership ledger
	// recorded but deliberately did not touch (ADR-0009 proof list).
	// Teardown reuses the field as the KEPT list (data retained without
	// purge flags) — one render path, two verbs, per the contract's
	// mirror phases install/uninstall.
	LeftAlone []string
	// Skipped lists with-reason refusals (teardown: two-key failures —
	// absent-from-snapshot, label mismatch, foreign identifier...).
	// Install leaves it empty.
	Skipped []string
	// Removed lists exactly what teardown deleted (install leaves it
	// empty): the ledger-diff proof list of ADR-0009's teardown.
	Removed []string
	// Checked lists verification outcomes ("dokku 0.38.32 ≥ 0.38",
	// "health: deployed+running").
	Checked []string

	// SecretsToProvision lists secret names the spec will want but no
	// provider can yet resolve (informational; empty in the M2 slice —
	// the controller's first reconcile reports them from M3 on).
	SecretsToProvision []string
	// Commands are the exact manual command equivalents this run printed
	// (sudo fallback, manual dokku commands; ADR-0009's print contract).
	Commands []string
	// NextCommands are the suggested next commands (contract phase 6).
	NextCommands []string
	// Notes carry non-list context for humans (adoption record note,
	// resume note, …).
	Notes []string

	// Preflight is the doctor report (always populated — the audit view
	// of phase 0, whatever later phases did).
	Preflight doctor.Report
}

// DataMap renders Result into the CLI envelope's Data payload.
func (r *Result) DataMap() map[string]any {
	m := map[string]any{
		"phasesRun":          r.PhasesRun,
		"phasesSkipped":      r.PhasesSkipped,
		"created":            r.Created,
		"adopted":            r.Adopted,
		"leftAlone":          r.LeftAlone,
		"skipped":            r.Skipped,
		"removed":            r.Removed,
		"checked":            r.Checked,
		"secretsToProvision": r.SecretsToProvision,
		"commands":           r.Commands,
		"nextCommands":       r.NextCommands,
		"notes":              r.Notes,
		"resumed":            r.Resumed,
	}
	return m
}
