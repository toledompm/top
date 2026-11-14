// SPDX-License-Identifier: MIT

package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// The phased runner: docs/contracts/installer.md phases 0-6 with resume.
//
// Resume discipline — every phase records ONE store operation (kind
// "install", details JSON {"phase": N, ...}); the NEWEST op per phase
// decides that phase's fate:
//
//	succeeded → skip (idempotent resume; re-running `topctl install`
//	            after a complete install is a no-op resume)
//	running/pending/failed or no record → redo, after aborting any
//	            non-terminal leftovers (a previous invocation died
//	            mid-phase; SIGKILL leaves no graceful trail).
//
// Phase 0 (preflight) is the deliberate exception: doctor is read-only
// and cheap, so it re-runs on EVERY invocation — an interrupted install's
// re-run re-verifies the host before touching anything again.
//
// Store details are fixed at InsertOperation (the update path moves state
// only), so each phase runs its work FIRST (anchored on the op's fresh
// UUID, which exists before any run), collects its details payload, and
// then records the full lifecycle pending → running → terminal. An
// interruption between work and record is safe: the next run redoes the
// phase (every phase body is idempotent — Ensure* verbs, ledger walks
// tolerate already-recorded rows).

// Phase identifiers as recorded in operation details.
const (
	phasePreflight       = 0
	phaseInventory       = 1
	phaseDokkuSetup      = 2
	phaseWiring          = 3
	phaseBootstrapDeploy = 4
	phaseFirstReconcile  = 5
	phaseReport          = 6
)

// phaseNames for op details and Result.PhasesRun rendering.
var phaseNames = map[int]string{
	phasePreflight:       "0-preflight",
	phaseInventory:       "1-inventory",
	phaseDokkuSetup:      "2-dokku-setup",
	phaseWiring:          "3-platform-wiring",
	phaseBootstrapDeploy: "4-bootstrap-deploy",
	phaseFirstReconcile:  "5-first-reconcile",
	phaseReport:          "6-report",
}

// maxDetailsSnapshotBytes caps the inventory snapshot embedded in an
// operation's details JSON (the op stays a fixed-small-size history row;
// honest degradation beats an oversized record). The full JSON ALSO lives
// under the data dir when it already exists (phase 1's documented "keep
// it inside TOP dirs" rule — never outside, which would be a host
// mutation).
const maxDetailsSnapshotBytes = 256 * 1024

// Installer pins (T-206): Dokku v0.38.32 was the newest 0.38.x when the
// constant landed (2026-10-27, verified against the docs page at review
// time; deliberately NOT network-verified by code or tests — no network
// in tier-1). Bump CONSCIOUSLY — release review, e2e matrix rerun; the
// doctor floor stays "0.38" (ADR-0001).
const (
	dokkuBootstrapScriptURL = "https://dokku.com/install/v0.38.32/bootstrap.sh"
	dokkuBootstrapTag       = "v0.38.32"
	bootstrapScriptFileName = "bootstrap.sh"
)

// wgetScriptCommand / sudoScriptCommands render the exact manual command
// equivalents of the gated Dokku bootstrap — the printable runbook
// ADR-0009 mandates (TOP never assumes passwordless sudo, never
// escalates; the operator's password prompt happens in THEIR terminal).
func wgetScriptCommand() string { return "wget -NP . " + dokkuBootstrapScriptURL }

func sudoScriptCommands() []string {
	return []string{
		wgetScriptCommand(),
		"sudo DOKKU_TAG=" + dokkuBootstrapTag + " bash " + bootstrapScriptFileName,
	}
}

// Install runs the phased installer (docs/contracts/installer.md). On any
// phase failure it returns the partial Result plus a typed error; prior
// phases stay untouched and the failing op records the redacted failure
// (bootstrap-failed marker for phase 4). A re-run resumes where the store
// says work is left.
func Install(ctx context.Context, opts Options) (*Result, error) {
	opts.applyDefaults()
	if err := validateRepoURL(opts.RepoURL); err != nil {
		return nil, err
	}
	if !opts.DryRun && !opts.Confirm {
		return nil, fmt.Errorf("mutating phases require the ADR-0009 approval marker: %w", ErrGateRequired)
	}
	if !opts.DryRun && opts.Backend == nil {
		return nil, fmt.Errorf("install: %w: Options.Backend is required when not --dry-run", domain.ErrInvalid)
	}

	res := &Result{Notes: []string{}}
	note := func(s string) {
		if opts.Progress != nil {
			opts.Progress(s)
		}
	}

	// Dry-run: the full plan, zero mutations — no store, no ledger, no
	// argv beyond read-only probes, no filesystem changes. Phase 0 runs
	// for real (read-only): the plan is honest about the host even under
	// a dry-run, and a failing preflight is a clean, argument-free
	// abort.
	if opts.DryRun {
		rep, perr := runPreflight(ctx, opts, nil) // no store: dry-run owns no ledger surface
		res.Preflight = rep
		if perr != nil {
			return res, perr
		}
		res.PhasesRun = dryRunPlan(opts)
		res.Notes = append(res.Notes,
			"dry-run: nothing was written; cross-check the plan, then re-run with --confirm",
			"adoption note: phase 5 (first reconciliation + ADR-0005 ownership handoff) lists as planned but writes nothing in a dry-run — the bootstrap deployment stays bootstrap-owned until the real run completes it")
		return res, nil
	}

	st, ownStore, err := openStore(ctx, opts.DataDir, opts.Store)
	if err != nil {
		return nil, err
	}
	if ownStore {
		defer func() { _ = st.Close() }()
	}
	if err := abortStaleInstallOps(ctx, st, opts.Now()); err != nil {
		return nil, err
	}

	// Phase 0 — preflight always re-runs (read-only). Recorded through
	// the same phase-op wrapper (its audit record exists like every
	// other phase's). Findings abort before anything mutates.
	if err := runPhase(ctx, st, opts, phasePreflight, res, func(opID string, res *Result, extra phaseExtras) error {
		note("▸ " + phaseNames[phasePreflight] + " — running")
		rep, perr := runPreflight(ctx, opts, st)
		res.Preflight = rep
		return perr
	}); err != nil {
		return res, err
	}
	// Phases 1..4 — resume-aware; a failure stops the sequence.
	done := map[int]bool{}
	if err := scanPhases(ctx, st, done); err != nil {
		return nil, err
	}

	var failure error
	skip := func(p int) {
		if done[p] {
			res.PhasesSkipped = append(res.PhasesSkipped, phaseNames[p])
			note("▸ " + phaseNames[p] + " — skipped (already complete)")
		}
	}

	if done[phaseInventory] {
		skip(phaseInventory)
	} else if err := runPhase(ctx, st, opts, phaseInventory, res, func(opID string, res *Result, extra phaseExtras) error {
		note("▸ " + phaseNames[phaseInventory] + " — running")
		return runInventory(ctx, opts, st, opID, res, extra)
	}); err != nil {
		failure = err
	}

	if failure == nil && done[phaseDokkuSetup] {
		skip(phaseDokkuSetup)
	} else if failure == nil {
		if err := runPhase(ctx, st, opts, phaseDokkuSetup, res, func(opID string, res *Result, extra phaseExtras) error {
			note("▸ " + phaseNames[phaseDokkuSetup] + " — running")
			return runDokkuSetup(ctx, opts, res)
		}); err != nil {
			failure = err
		}
	}

	if failure == nil && done[phaseWiring] {
		skip(phaseWiring)
	} else if failure == nil {
		if err := runPhase(ctx, st, opts, phaseWiring, res, func(opID string, res *Result, extra phaseExtras) error {
			note("▸ " + phaseNames[phaseWiring] + " — running")
			return runWiring(ctx, opts, st, opID, res, extra)
		}); err != nil {
			failure = err
		}
	}

	if failure == nil && done[phaseBootstrapDeploy] {
		skip(phaseBootstrapDeploy)
	} else if failure == nil {
		if err := runPhase(ctx, st, opts, phaseBootstrapDeploy, res, func(opID string, res *Result, extra phaseExtras) error {
			note("▸ " + phaseNames[phaseBootstrapDeploy] + " — running")
			return runBootstrapDeploy(ctx, opts, st, opID, res, extra)
		}); err != nil {
			failure = err
		}
	}

	// Phase 5 — first reconciliation + ADR-0005 ownership handoff
	// (T-402, internal/install/adopt.go). Resume-aware like phases 1..4;
	// the completion key is the adoption record, not the op state alone
	// (scanPhases): a phase that found no controller spec succeeded with
	// "adopted": false and redoes on the next install.
	if failure == nil && done[phaseFirstReconcile] {
		skip(phaseFirstReconcile)
	} else if failure == nil {
		note("▸ " + phaseNames[phaseFirstReconcile] + " — running")
		if err := runPhase(ctx, st, opts, phaseFirstReconcile, res, func(opID string, res *Result, extra phaseExtras) error {
			return runFirstReconcile(ctx, opts, st, opID, res, extra)
		}); err != nil {
			failure = err
		}
	}

	// Phase 6 — report always runs (even after a failure: the Result
	// carries the partial lists; this op marks the invocation itself).
	// The body also writes the runtime config when absent (the operator
	// should not hand-maintain what install already knows: git URL,
	// namespaces, poll cadence — T-600 feedback, live homelab 2026-11-14).
	repErr := runPhase(ctx, st, opts, phaseReport, res, func(opID string, res *Result, extra phaseExtras) error {
		// Resume paths can reach phase 6 with the config still absent
		// (phase 4 skipped over an old completion) — idempotent if-exists.
		if err := writeRuntimeConfigIfAbsent(opts, res); err != nil {
			return err
		}
		return nil // the report content IS the Result
	})
	if err := firstErr(failure, repErr); err != nil {
		return res, err
	}

	res.NextCommands = []string{
		"topctl diff",
		"topctl doctor",
	}
	res.Resumed = len(res.PhasesSkipped) > 0
	return res, nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// phaseExtras is the phase's completed-work payload for the operation's
// details JSON (closure-filled; JSON-marshalable values only).
type phaseExtras map[string]any

// --- plan rendering -----------------------------------------------------------

// dryRunPlan renders the planned phases as the phased list.
func dryRunPlan(opts Options) []string {
	return []string{
		"0-preflight: run doctor + installer checks (read-only)",
		"1-inventory: snapshot host state → ledger as pre-existing (no writes in dry-run)",
		"2-dokku-setup: " + dokkuSetupPlan(opts),
		"3-platform-wiring: dirs " + dirsPlan(opts.DataDir) + "; network " + InternalNetwork + "; deploy key: " + deployKeyPlan(opts) + repoInitPlan(opts),
		"4-bootstrap-deploy: storage entry " + StorageEntryName + " → app " + opts.ControllerAppName() + " ← " + opts.ImageRef() + "; bounded health wait",
		"5-first-reconcile: one reconciler pass over the seeded repo specs + the ADR-0005 ownership handoff (core-top-controller bootstrap-owned → managed) — no redeploy (dry-run and no handoff wired: adoption is planned only)",
		"6-report: created/adopted/left-alone/checked + next commands",
	}
}

func dokkuSetupPlan(opts Options) string {
	if !opts.InstallDokku {
		return "verify dokku presence/health (refusing to install without --install-dokku)"
	}
	return "if absent: pinned upstream bootstrap (" + wgetScriptCommand() + " / sudo DOKKU_TAG=" + dokkuBootstrapTag + " bash " + bootstrapScriptFileName + ") after the printed change list; else verify version ≥ " + doctor.MinDokkuVersion
}

func dirsPlan(dataDir string) string {
	return dataDir + "/{,secrets,git} as 0700 dirs (files 0600)"
}

func deployKeyPlan(opts Options) string {
	if opts.DeployKey == "" {
		return "operator-supplied path REQUIRED in v0 (generation is M3)"
	}
	return "copy operator-supplied " + opts.DeployKey + " → <dataDir>/git/deploy_key (0600)"
}

// repoInitPlan appends the --init clause to the printed phase-3 plan (dry-
// run only mutates nothing; the plan is honest about what the real run
// would push).
func repoInitPlan(opts Options) string {
	if !opts.Init {
		return ""
	}
	cred := "the one-time credential from the environment (TOP_INIT_TOKEN), or the deploy-key path for ssh remotes"
	return "; --init: if the remote resolves EMPTY, seed the embedded platform-repo template and push ONE commit (identity " +
		initAuthorNameDefault + " <" + initAuthorEmailDefault + ">; " + cred + "; never creates the remote)"
}

// --- the uniform phase-operation wrapper ----------------------------------------

// runPhase is the uniform per-phase operation-record wrapper: the work
// runs anchored on a fresh op UUID; on completion the audit record is
// appended (pending → running → terminal, with the redacted failure when
// the work failed) and the Result's phase list updated. The operation's
// Details payload carries the phase metadata plus whatever `extra` grew
// during the work (the phase-1 snapshot).
func runPhase(
	ctx context.Context,
	st store.Store,
	opts Options,
	phase int,
	res *Result,
	work func(opID string, res *Result, extra phaseExtras) error,
) error {
	phaseName := phaseNames[phase]
	op, err := domain.NewOperation(domain.OpInstall, domain.ActorUser, "", "")
	if err != nil {
		return fmt.Errorf("phase %s: create op: %w", phaseName, err)
	}

	extra := phaseExtras{}
	workErr := work(op.ID.String(), res, extra)

	op.Details = phaseDetails(phase, opts, extra)
	if err := st.InsertOperation(ctx, op); err != nil {
		return fmt.Errorf("phase %s: record insert: %w", phaseName, err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", opts.Now()); err != nil {
		return fmt.Errorf("phase %s: record running: %w", phaseName, err)
	}
	var terminal error
	if workErr != nil {
		terminal = st.UpdateOperationState(ctx, op.ID, domain.OpFailed, renderPhaseFailure(phaseName, workErr), opts.Now())
	} else {
		terminal = st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", opts.Now())
	}
	if terminal != nil {
		return fmt.Errorf("phase %s: record terminal: %w", phaseName, terminal)
	}
	if workErr != nil {
		if phase == phaseBootstrapDeploy {
			return fmt.Errorf("phase %s: bootstrap-failed: %w", phaseName, workErr)
		}
		return fmt.Errorf("phase %s: %w", phaseName, workErr)
	}
	res.PhasesRun = append(res.PhasesRun, phaseName)
	return nil
}

// renderPhaseFailure prefixes the redacted failure text with the
// bootstrap-failed marker when the failing phase is the bootstrap deploy
// so the failure is visible in `topctl history`) and always carries the
// resume command.
func renderPhaseFailure(phaseName string, err error) string {
	prefix := ""
	if phaseName == phaseNames[phaseBootstrapDeploy] {
		prefix = "bootstrap-failed: "
	}
	return prefix + strings.TrimSpace(err.Error()) +
		" (resume: re-run `topctl install <repo-url>` — completed phases are skipped)"
}

// phaseDetails is the details JSON shape for one install phase op.
func phaseDetails(phase int, opts Options, extra map[string]any) map[string]any {
	d := map[string]any{
		"phase":     phase,
		"phaseName": phaseNames[phase],
		"repo":      opts.RepoURL,
		"branch":    opts.Branch,
		"dataDir":   opts.DataDir,
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// --- store plumbing ----------------------------------------------------------

// openStore honors an injected store; otherwise opens (and, when owned,
// later closes) the state file at <dataDir>/top.db — the doctor's
// documented path, kept identical so doctor finds the same file.
func openStore(ctx context.Context, dataDir string, injected store.Store) (store.Store, bool, error) {
	if injected != nil {
		return injected, false, nil
	}
	path := dataDir + "/" + "top.db"
	s, oerr := sqlite.Open(ctx, path)
	if oerr != nil {
		return nil, false, fmt.Errorf("%w: %s: %v", ErrStoreUnavailable, path, oerr)
	}
	return s, true, nil
}

// abortStaleInstallOps marks non-terminal install ops aborted — a previous
// invocation died mid-phase (SIGKILL leaves no failed marker); redoing the
// phase is safe and is what the resume discipline prescribes.
func abortStaleInstallOps(ctx context.Context, st store.Store, now time.Time) error {
	ops, err := st.ListOperations(ctx, 100, &store.OperationFilter{Kind: domain.OpInstall})
	if err != nil {
		return fmt.Errorf("aborting stale install ops: %w", err)
	}
	for _, op := range ops {
		if op.State == domain.OpPending || op.State == domain.OpRunning {
			if err := st.UpdateOperationState(ctx, op.ID, domain.OpAborted, "superseded by a new install run", now); err != nil {
				return fmt.Errorf("abort stale install op %s: %w", op.ID, err)
			}
		}
	}
	return nil
}

// scanPhases fills `done` from the store: the newest op per phase decides
// (ListOperations returns newest-first; the first op seen per phase wins).
//
// Phase 5's completion is keyed on the adoption record, not the op state
// alone: an install that could not adopt (no controller spec yet in the
// repo, or nothing to adopt) records SUCCESS with details "adopted":
// false — and its re-run REDOES the phase, so a controller spec pushed
// later still gets adopted. A phase that adopted (details "adopted":
// true, or a skip reason recorded as already-managed) is permanently
// done: re-running after a successful adoption re-adopts nothing (the
// T-402 op-record guard).
func scanPhases(ctx context.Context, st store.Store, done map[int]bool) error {
	ops, err := st.ListOperations(ctx, 100, &store.OperationFilter{Kind: domain.OpInstall})
	if err != nil {
		return fmt.Errorf("scanning install phase state: %w", err)
	}
	for _, op := range ops {
		p, ok := phaseOf(op)
		if !ok || done[p] {
			continue
		}
		if p == phaseFirstReconcile && op.State == domain.OpSucceeded {
			done[p] = op.Details["adopted"] == true
			continue
		}
		done[p] = op.State == domain.OpSucceeded
	}
	return nil
}

func phaseOf(op *domain.Operation) (int, bool) {
	v, ok := op.Details["phase"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64: // JSON round-trip
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

// --- phase 0: preflight -------------------------------------------------------

// runPreflight runs doctor (contract phase 0) plus the installer's own
// checks (repo reachable and non-empty per ADR-0005's no-init rule,
// deploy-key material paths for phase 3). Read-only. The call only fails
// after the findings are recorded (one run shows every fix at once),
// with the typed ErrPreflightFailed.
func runPreflight(ctx context.Context, opts Options, st store.Store) (doctor.Report, error) {
	rep := doctor.Run(ctx, doctor.Options{
		Runner:             opts.Runner,
		DataDir:            opts.DataDir,
		ReservedNamespaces: opts.Namespaces,
		SkipPorts:          opts.SkipPorts,
		OSProbe:            opts.OSProbe,
		Probe:              opts.Probe,
		MemProbe:           opts.MemProbe,
		DiskProbe:          opts.DiskProbe,
		OSReleasePath:      opts.OSReleasePath,
	})
	// The name-collision firewall is LEDGER-AWARE on the re-install path:
	// installation.md promises "re-running topctl install (same command,
	// same flags) resumes or skips at the completed phases" — a TOP-owned
	// core-top-controller row (bootstrap-owned or managed) is that OWN
	// leftover and must not abort an idempotent re-install (tier-4
	// verified 2026-11-03: the flat-fail made every re-install refuse).
	// An unledgered collision stays a hard failure per ADR-0009.
	if st != nil {
		downgradeLedgeredControllerName(ctx, st, &rep)
	}

	// Installer-specific preflight checks (doctor stays generic).
	addErrFinding(&rep, "repo/reachable", checkRepo(ctx, opts))
	addErrFinding(&rep, "install-input", checkDeployKeyPaths(opts))

	if !rep.OK() {
		return rep, fmt.Errorf("%w: %s", ErrPreflightFailed, rep.Summary())
	}
	return rep, nil
}

// downgradeLedgeredControllerName relaxes the doctor's
// names/top-controller failure when the CHECKED app is TOP's own
// ledgered deployment (bootstrap-owned or managed row). Anything else keeps
// the finding untouched.
func downgradeLedgeredControllerName(ctx context.Context, st store.Store, rep *doctor.Report) {
	if rep.OK() {
		return
	}
	entries, err := st.ListLedger(ctx, inventory.KindApp)
	if err != nil || len(entries) == 0 {
		return // unreadable ledger never widens the preflight surface
	}
	for _, e := range entries {
		if e.Identifier != doctor.ControllerAppName ||
			(e.State != store.ResourceManaged && e.State != store.ResourceBootstrapOwned) {
			continue
		}
		for i := range rep.Findings {
			f := rep.Findings[i]
			if f.ID != "names/top-controller" || f.Severity != doctor.SeverityFail {
				continue
			}
			converted := f
			converted.Severity = doctor.SeverityInfo
			converted.Message = "TOP's ledgered own controller deployment exists (" +
				string(e.State) + "): idempotent/resume install; adoption is already-managed or re-verifies"
			converted.Hint = "phase 4 adopts the existing deployment; `topctl uninstall` when the host is being handed back"
			rep.Findings[i] = converted
		}
	}
}

// addErrFinding turns one installer check error into a fail finding in
// the doctor vocabulary, so the phase-0 report stays one shape.
func addErrFinding(r *doctor.Report, id string, err error) {
	if err == nil {
		return
	}
	f := doctor.Finding{ID: id, Severity: doctor.SeverityFail, Check: "installer checks", Message: err.Error()}
	var repo *RepoErr
	if errors.As(err, &repo) {
		f.Hint = repo.Hint
	} else {
		f.Hint = "re-run `topctl install --help` for the option surface"
	}
	r.Findings = append(r.Findings, f)
}

// RepoErr is the typed repo finding (its message doubles as the clean
// preflight error ADR-0005 mandates for missing/empty repos).
type RepoErr struct {
	Message string
	Hint    string
}

func (e *RepoErr) Error() string { return e.Message }

// checkRepo validates the platform repo: reachable, branch non-empty
// (`git ls-remote <url> <branch>`; read-only). Missing/unreachable →
// the clean, instruction-bearing error in BOTH modes: --init pushes into a
// remote the operator created empty — topctl never creates the remote
// (provider-API creation is deliberately v0-deferred, ADR-0005 "provider
// permitting"). Empty → an error without --init (ADR-0005's existing-repo
// rule; install changes nothing on the remote), but the sanctioned
// precondition of `--init` (the seed flow, internal/install/repo_init.go).
// sshProbeEnv renders the child environment for the phase-0 repo probe:
// strict GIT_SSH_COMMAND over the operator's deploy key for ssh remotes
// (same transport vocabulary as repo_init's push; the key file is
// referenced BY PATH — TOP never reads it). HTTPS/file remotes need NO
// child environment (empty → even an env-less Runner can carry the probe).
func sshProbeEnv(opts Options) ([]string, error) {
	if !isSSHRemote(opts.RepoURL) {
		return nil, nil
	}
	if opts.DeployKey == "" || opts.KnownHosts == "" {
		return nil, fmt.Errorf("install: ssh remote needs --deploy-key and --known-hosts for the reachability probe")
	}
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=" + gitsource.SSHCommand(opts.DeployKey, opts.KnownHosts)}, nil
}

func checkRepo(ctx context.Context, opts Options) error {
	url, branch := opts.RepoURL, opts.Branch
	// SSH remotes authenticate via the operator's deploy key (ADR-0008):
	// the probe rides the SAME GIT_SSH_COMMAND transport phase 3's --init
	// push uses — otherwise a root/sudo context (where the operator's
	// interactive ssh config does not apply) probes github with no
	// identity and fails loudly with "unreachable" while the repo is
	// perfectly readable. HTTPS remotes need no ssh transport.
	// BUG HISTORY: found 2026-11-13 live on the first homelab install
	// (media-server): `sudo topctl install --init --deploy-key …` failed
	// preflight with an unnamed repo/reachable failure while the same
	// command worked from the operator shell.
	env, err := sshProbeEnv(opts)
	if err != nil {
		return err
	}
	_, _, err = envRunnerOf(opts.Runner).RunEnv(ctx, env, "git", "ls-remote", url, branch)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return &RepoErr{
			Message: "git is not on PATH; the platform repo cannot be verified",
			Hint:    "install git (the gated dokku bootstrap brings it along, or `sudo apt-get install -y git`)",
		}
	case err != nil:
		hint := "create the repo (empty is fine for --init) and push at least one commit to " + branch + ", or correct the URL. TOP never auto-creates the remote (ADR-0005: provider-API creation is a documented v0 deferral)."
		if !opts.Init {
			hint = "create the repo and push at least one commit to " + branch + ", or correct the URL — or, with an EMPTY remote: re-run with --init to seed TOP's platform-repo template (ADR-0005)."
		}
		return &RepoErr{
			Message: fmt.Sprintf("platform repo %s is not reachable: %v", url, err),
			Hint:    hint,
		}
	}
	// Exit 0: for the branch pattern, exit-0 with empty stdout means the
	// repo answered but no ref matched — empty repo / no commits on the
	// branch.
	out, _, _ := envRunnerOf(opts.Runner).RunEnv(ctx, env, "git", "ls-remote", url, branch)
	if strings.TrimSpace(string(out)) == "" {
		if opts.Init {
			return nil // empty is exactly what --init seeds (freshly re-verified in phase 3)
		}
		return &RepoErr{
			Message: fmt.Sprintf("platform repo %s resolved, but branch %q has no commits (empty) — refusing to bootstrap from it", url, branch),
			Hint:    "push the initial platform-repo content (services, spec) and re-run — or, for a greenfield host, re-run with --init: TOP seeds the embedded platform-repo template and pushes one commit (ADR-0005; the install never silently auto-creates).",
		}
	}
	return nil
}

// checkDeployKeyPaths validates the operator-supplied key-material paths
// (phase 3's provisioning inputs; v0 = path (a) only — operator points at
// files; subprocess `ssh-keygen` generation is deliberately deferred with
// an explicit TODO to M3, because inventing keys is a bigger consent than
// pointing at an existing one).
func checkDeployKeyPaths(opts Options) error {
	if opts.DeployKey == "" {
		return fmt.Errorf("--deploy-key is required in v0: point it at the private key file already authorized with the provider; key GENERATION (via ssh-keygen) is deferred to M3")
	}
	if err := readableFile(opts.DeployKey); err != nil {
		return fmt.Errorf("--deploy-key %s: %w", opts.DeployKey, err)
	}
	if opts.KnownHosts == "" {
		return fmt.Errorf("--known-hosts is required in v0: point it at the pinned known_hosts file for the git provider host; unpinned host keys are never accepted")
	}
	if err := readableFile(opts.KnownHosts); err != nil {
		return fmt.Errorf("--known-hosts %s: %w", opts.KnownHosts, err)
	}
	return nil
}

func readableFile(path string) error {
	st, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("file does not exist")
	case err != nil:
		return err
	case st.IsDir():
		return fmt.Errorf("is a directory, want a file")
	}
	return nil
}

// validateRepoURL refuses non-URL-ish repo arguments early (the CLI's
// usage gate): scheme'd URLs or scp-like "git@host:path" only — "does the
// arg plausibly name a remote" is the check; reachability is phase 0's job.
// file:// is accepted: a filesystem git remote is a legitimate transport
// (and the tier-1 test double for a provider remote — no containers).
var repoURLOrSCP = regexp.MustCompile(`^(https?://|file://|ssh://|git://)|^[^@\s]+@[^:\s]+:[^\s]+$`)

func validateRepoURL(url string) error {
	if url == "" {
		return fmt.Errorf("install: %w: empty repo URL", domain.ErrInvalid)
	}
	if !repoURLOrSCP.MatchString(strings.TrimSpace(url)) {
		return fmt.Errorf("install: %w: repo %q does not look like a git remote (want https://…, ssh://…, or git@host:path)", domain.ErrInvalid, url)
	}
	return nil
}
