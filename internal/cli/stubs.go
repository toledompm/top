package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/controller"
	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/install"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/secrets"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store/sqlite"
)

// strings is used by warnSpecText / readinessHints below. (kept out of the
// import-group comment above deliberately: stdlib goes first)

// orRunner resolves ONE shared Runner for a command body: Options.Runner
// (tests inject the fake), else the production FastExec. Compositions
// (install + its dokku backend) share the same value so a test's fake
// sees every argv.
func (c *cctx) orRunner() exec.Runner {
	if c.runner != nil {
		return c.runner
	}
	return exec.NewFastExec()
}

// Command bodies (the file keeps its M1 name; most bodies are real now).
// What remains a *validated stub* here: `status`.
// Real bodies: doctor, install, uninstall, logs, controller run,
// plus the T-309 reconcile family in reconcmd.go. Stubs parse and
// validate flags and arguments against internal/domain, load config
// (root persistentPreRun), and print the versioned envelope; no stub
// touches the store, exec layer, or host. See internal/cli/README.md for
// how the remaining bodies get their service wiring behind the identical
// flag surface.

// newVersionCmd is the one command that is fully real in M1: it prints
// build info and support floors (read-only, exit 0).
func newVersionCmd(c *cctx) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version, output schemaVersion, and minimum supported Dokku version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.emitOk(cmd, "", map[string]any{
				"topctl":        c.opts.Version,
				"schemaVersion": JSONSchemaVersion,
				"minDokku":      MinDokkuVersion,
			})
		},
	}
}

// newDoctorCmd — real (T-203): read-only host preflight, installer
// phase 0. Runs internal/doctor with TOP's config (dataDir + reserved
// namespaces) and maps: fails → exit 1 (diagnose-findings row in
// docs/contracts/cli.md), warn/info/ok → 0; --skip-ports opts out of
// the 80/443 bind probe.
func newDoctorCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host preconditions (pre-install environment check)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := doctor.Run(cmd.Context(), doctor.Options{
				DataDir:            c.cfg.DataDir,
				ReservedNamespaces: c.cfg.Namespaces.Reserved,
				SkipPorts:          flagBool(cmd, "skip-ports"),
			})
			// text: the full human report (findings + hints, ADR-0009);
			// json: the versioned envelope carrying the payload.
			if c.format == FormatJSON {
				if err := c.emitOk(cmd, rep.Summary(), rep.EnvelopeData()); err != nil {
					return err
				}
			} else if err := rep.RenderText(c.out); err != nil {
				return &ExitError{Code: ExitFailed, Err: err}
			}
			if !rep.OK() {
				return &ExitError{
					Code: ExitDrift,
					Err:  errors.New("doctor: host preconditions unsatisfied"),
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("skip-ports", false,
		"skip the read-only 80/443 availability probe (bind-close check)")
	return cmd
}

// newInstallCmd — REAL (T-205..T-207): the phased installer behind the
// same flag surface the stub validated. Wiring mirrors the doctor
// bodies: streams+config from the cctx, Runner from Options (fake in
// tests), the concrete backend wired HERE (composition root — internal/
// install only knows the PlatformBackend interface, ADR-0006). Exit codes:
// preflight findings → 1 (drift space, doctor's row); gate/sudo refusals
// and phase failures → 3; store unavailability → 4; usage → 2. --dry-run
// prints the plan, mutates nothing (ADR-0009 dry-run requirement).
//
// NOTE (M-scope): --init (repo seeding from the embedded template into an
// EMPTY remote) is REAL since T-212; deploy-key GENERATION is M3 (the
// operator's paths are still required in v0). The interactive TTY is
// the CLI default — requireConfirm prompts on a terminal and --confirm is
// the deterministic path; the installer itself never prompts.
func newInstallCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <repo>",
		Short: "Install TOP on this host from a platform repo (confirm-gated)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.requireConfirm(cmd, "installing TOP on this host"); err != nil {
				return err
			}
			runner := c.orRunner()
			opts := install.Options{
				Runner:  runner,
				Backend: dokku.New(runner),
				DataDir: c.cfg.DataDir,
				RepoURL: args[0],
				Branch:  c.cfg.Git.Branch,
				// Live progress: one stderr line per phase (text mode)
				// — the operator sees what's running instead of a silent
				// minute (T-600 flair, 2026-11-14).
				Progress: func(stage string) {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "topctl install: "+stage)
				},
				InstallDokku:    flagBool(cmd, "install-dokku"),
				Confirm:         true, // requireConfirm above IS the gate
				DryRun:          flagBool(cmd, "dry-run"),
				DeployKey:       flagString(cmd, "deploy-key"),
				KnownHosts:      flagString(cmd, "known-hosts"),
				ControllerImage: flagString(cmd, "controller-image"),
				ControllerTag:   flagString(cmd, "controller-tag"),
				// --init surface (T-212, ADR-0005): seed the embedded
				// platform-repo template into an EMPTY remote. The one-time
				// write credential is read from the environment AT RUNTIME
				// ONLY — never a flag/argv value, never stored, never
				// logged (ADR-0008/0005 credential model).
				Init:        flagBool(cmd, "init"),
				InitToken:   os.Getenv("TOP_INIT_TOKEN"),
				InitVersion: c.opts.Version,
				Namespaces:  c.cfg.Namespaces.Reserved,
				SkipPorts:   flagBool(cmd, "skip-ports"),
			}
			// T-406: wire phase 5's reconciliation handoff (ADR-0005
			// adoption). DryRun touches nothing, so it runs with the
			// handoff unwired, exactly as installs before this wiring
			// recorded their skip finding. No eager I/O happens here: the
			// handoff opens its own state connection inside FirstRun.
			if !flagBool(cmd, "dry-run") {
				opts.FirstRun = c.wireFirstRun(args[0])
			}
			res, err := install.Install(cmd.Context(), opts)
			if err != nil {
				return c.installExit(cmd, res, err)
			}
			return c.emitInstallResult(cmd, res)
		},
	}
	cmd.Flags().Bool("install-dokku", false, "also install Dokku if absent (approval-gated change list: apt packages, nginx on 80/443, dokku user, sshcommand)")
	cmd.Flags().Bool("skip-ports", false, "skip the read-only 80/443 availability probe in preflight")
	cmd.Flags().Bool("init", false, "if the platform repo resolves EMPTY: seed the embedded platform-repo template and push ONE initial commit (one-time credential via the TOP_INIT_TOKEN environment — never a flag value, never stored; the remote itself is never created)")
	cmd.Flags().String("deploy-key", "", "path to the operator-supplied private deploy key (provisioned 0600 into <dataDir>/git; required in v0)")
	cmd.Flags().String("known-hosts", "", "path to the pinned known_hosts file (provisioned 0600 into <dataDir>/git; required in v0)")
	cmd.Flags().String("controller-image", "", "override the pinned bootstrap controller image (default: internal/install's ghcr.io/example/top/topctl)")
	cmd.Flags().String("controller-tag", "", "override the pinned bootstrap controller tag (default: the installer pin)")
	return cmd
}

// emitInstallResult renders a completed Result (envelope on JSON; text
// report for humans).
func (c *cctx) emitInstallResult(cmd *cobra.Command, res *install.Result) error {
	if c.format == FormatJSON {
		return c.emitOk(cmd, install.Summary(res), res.DataMap())
	}
	if err := install.RenderResultText(c.out, res); err != nil {
		return &ExitError{Code: ExitFailed, Err: err}
	}
	return nil
}

// installExit maps an install failure (partial Result retained) onto the
// cli contract's exit codes: preflight/decisions-with-findings → 1 (the
// drift/diagnose space), gate/sudo refusals + phase failures → 3, store
// unavailability → 4.
func (c *cctx) installExit(cmd *cobra.Command, res *install.Result, err error) error {
	switch {
	case errors.Is(err, install.ErrPreflightFailed), errors.Is(err, install.ErrNothingInstalled):
		return c.emitErrCode(cmd, "preflight-failed", ExitDrift, err.Error())
	case errors.Is(err, install.ErrRepoNotEmpty), errors.Is(err, install.ErrRemoteUnreachable):
		// Decision-with-evidence refusals (both leave the remote
		// untouched): --init on foreign repo content, or an unreachable
		// remote. Evidence is drift-class, not crash-class (exit 1).
		return c.emitErrCode(cmd, "repo-state", ExitDrift, err.Error())
	case errors.Is(err, install.ErrStoreUnavailable):
		return c.emitErrCode(cmd, "store-unavailable", ExitUnreachable, err.Error())
	case errors.Is(err, domain.ErrInvalid):
		return c.usageErr(cmd, err.Error())
	default:
		if res != nil {
			_ = install.RenderResultText(c.out, res) // partial progress still prints
		}
		return c.emitErrCode(cmd, "failed", ExitFailed, err.Error())
	}
}

// newDiffCmd — REAL (T-309): read-only computed diff over the resolved
// platform-repo revision (zero side effects: no lease, no store write —
// the store opens only when <dataDir>/top.db already exists, and only for
// the orphan-candidate ledger read). Consumes internal/reconcile's
// DiffOnly — the SAME plan the next reconcile runs would execute
// (reconciler.md parity). Exit: 1 when drift/orphans exist (the cli.md
// "drift/diagnose-findings" row — the idempotency anchor: after a green
// run the same diff exits 0), 0 clean, 4 git/store/backend unavailable,
// 3 invalid specs, 2 bad flags.
func newDiffCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Desired vs observed services (zero side effects; exit 1 on drift)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rev := flagString(cmd, "revision")
			if rev != "" {
				if err := domain.ValidRevision(rev); err != nil {
					return c.usageErr(cmd, "--revision: "+err.Error())
				}
			}
			ctx := cmd.Context()
			st, err := c.orStore(ctx)
			if err != nil {
				return c.storeExit(cmd, err)
			}
			d, err := c.resolveDesired(ctx, rev)
			if err != nil {
				return c.resolveExit(cmd, err)
			}
			defer func() { _ = d.snap.Close() }()
			// A diff is NEVER prune-allowed: delete rows are "prune only".
			rep, err := reconcile.DiffOnly(ctx, c.runInput(d, false, false), dokku.New(c.orRunner()), st)
			if err != nil {
				return c.resolveExit(cmd, err)
			}
			drift, err := c.renderDiff(cmd, rep)
			if err != nil {
				return err
			}
			if drift {
				return &ExitError{Code: ExitDrift, Err: errors.New("diff: drift present")}
			}
			return nil
		},
	}
	cmd.Flags().String("revision", "", "diff against a specific git commit sha instead of HEAD")
	return cmd
}

// newReconcileCmd — REAL (T-309): one reconciler pipeline run over the
// resolved platform repo. Gates per ADR-0009's consent vocabulary:
// --prune and --include-controller each require their own confirmation
// (and there is no yes-to-everything); destructive pairs bind here —
// `--prune` without `--confirm` non-interactively is exit 2 (requireConfirm),
// and the reconciler re-asserts the consent pair internally. Exit codes
// (docs/contracts/cli.md table): 0 succeeded; 3 failed runs (including
// contained per-service failures, lease conflicts with code
// "lease-conflict", validation failures with code "validation-failed");
// 4 git/store/backend unavailability; 2 usage. --now is accepted: the
// manual CLI run is always immediate — lock backoff belongs to the
// controller loops (T-310). --include-controller's confirmation covers
// the ADR-0005 self service; until the M4 adopt path the controller is
// REPORT-ONLY (skipped/self-drift-reported), so confirming it currently
// admits nothing beyond the report row it already gets.
func newReconcileCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Run the reconciler across namespaces (or one service)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var svcRef domain.ServiceRef
			if svc := flagString(cmd, "service"); svc != "" {
				var err error
				if svcRef, err = domain.ParseServiceRef(svc); err != nil {
					return c.usageErr(cmd, "--service: "+err.Error())
				}
			}
			prune, includeCtrl := flagBool(cmd, "prune"), flagBool(cmd, "include-controller")
			if prune || includeCtrl {
				if err := c.requireConfirm(cmd, "reconcile --prune / --include-controller deletes or changes workloads"); err != nil {
					return err
				}
			}
			// requireConfirm returned nil: either --confirm was present,
			// or a human consented at the prompt — both count as the
			// --confirm side of the consent pair the pipeline enforces.
			confirm := flagBool(cmd, "confirm")
			if (prune || includeCtrl) && !confirm {
				confirm = true
			}
			ctx := cmd.Context()
			st, err := c.orStore(ctx)
			if err != nil {
				return c.storeExit(cmd, err)
			}
			d, err := c.resolveDesired(ctx, "")
			if err != nil {
				return c.resolveExit(cmd, err)
			}
			defer func() { _ = d.snap.Close() }()
			in := c.runInput(d, prune, confirm)
			if !svcRef.IsZero() { // --service: the targeted single-service run (same subset rule as deploy)
				in.Specs, err = deploySpecs(d.specs, svcRef)
				if err != nil {
					return c.usageErr(cmd, err.Error()) // unknown service/dependency: the operator's ask
				}
			}

			if flagBool(cmd, "dry-run") { // validated refs already done above
				rep, err := reconcile.DiffOnly(ctx, in, dokku.New(c.orRunner()), st)
				if err != nil {
					return c.resolveExit(cmd, err)
				}
				return c.renderDryPlan(cmd, d, rep)
			}
			return c.execReconcile(cmd, d, c.runInput(d, prune, confirm), st)
		},
	}
	cmd.Flags().Bool("now", false, "reconcile immediately instead of honoring locks/backoff (accepted; the CLI run is always immediate — backoff is the controller loop's, T-310)")
	cmd.Flags().String("service", "", "reconcile a single service (\"ns/name\"; bare \"name\" = default/name)")
	cmd.Flags().Bool("prune", false, "delete services observed but absent from the spec (confirm-gated, ADR-0009 consent pair)")
	cmd.Flags().Bool("include-controller", false, "include TOP's own controller in the reconcile (confirm-gated, ADR-0005; adopted-self revision drift stays human-gated — T-402/M4)")
	return cmd
}

// renderDryPlan prints a dry-run plan (cli.md: "Every mutating command
// supports --dry-run printing the plan") and exits 0: the plan IS the
// mutation's preview, its exit code is not the diff command's script
// channel (drift signaling belongs to `topctl diff`).
func (c *cctx) renderDryPlan(cmd *cobra.Command, d desired, rep reconcile.DiffReport) error {
	if c.format == FormatJSON {
		if err := c.emitOk(cmd, "dry-run: plan printed, nothing applied", diffData(rep)); err != nil {
			return err
		}
		return nil
	}
	if err := warnSpecText(c.out, d.warnings); err != nil {
		return err
	}
	if err := renderDiffText(c.out, rep); err != nil {
		return &ExitError{Code: ExitFailed, Err: err}
	}
	return nil
}

// warnSpecText prints the non-fatal spec warnings (mutable tags, latest)
// — they are advisory and must not fail a run (internal/spec).
func warnSpecText(w io.Writer, warns []spec.Warning) error {
	var b strings.Builder
	for _, wn := range warns {
		fmt.Fprintf(&b, "  warning: %s: %s (%s)\n", wn.Path, wn.Message, wn.Code)
	}
	if b.Len() > 0 {
		if _, err := w.Write([]byte(b.String())); err != nil {
			return fmt.Errorf("writing warnings: %w", err)
		}
	}
	return nil
}

// newDeployCmd — REAL (T-309): docs/contracts/cli.md deploy row —
// "topctl deploy <ns/name> [--revision sha] | mutating | Reconciler
// (single service)". One targeted converge: the named service plus its
// transitive dependencies (deploySpecs — the pipeline must not fail a
// deploy because an unresolved dependsOn was never deployed), reconciled
// toward the resolved platform-repo revision (--revision deploys a pinned
// sha; default resolves the configured branch head). The run is a plain
// non-prune reconcile subset: same pipeline (validate → lease → plan →
// apply → gate → record) and exit mapping as `topctl reconcile`;
// --dry-run prints the same shared plan and mutates nothing (cli.md
// invariant). Deploy is NOT confirm-gated: it converges toward git
// (ADR-0002), unlike prune deletions — the reconciler's consent pair stays
// the destructive boundary.
// The self-deploy exception (T-406): `topctl deploy core/top-controller`
// is THE ADR-0005 human gate the reconciler's own runs point at — so the
// command confirm-gates exactly that target (requireConfirm: --confirm or
// the interactive self-announcement prompt; non-TTY without --confirm →
// exit 2 like every gated mutation) and propagates RunInput
// .SelfDeployApproved, which converts the reconciler's report-only self
// refusal into the explicit-human apply. The output echoes the contract's
// old-release preservation fact and the rollback path on both success and
// failure (echoed BEFORE the run runs, so a dead controller still printed
// it).
func newDeployCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <ns/name>",
		Short: "Deploy one service to its desired (or given) revision",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := domain.ParseServiceRef(args[0])
			if err != nil {
				return c.usageErr(cmd, err.Error())
			}
			selfDeploy := ref.Equal(reconcile.SelfRef)
			if selfDeploy {
				if err := c.requireConfirm(cmd, "deploying TOP's own controller (self-update, human-gated per ADR-0005)"); err != nil {
					return err
				}
				if c.format != FormatJSON {
					_, _ = fmt.Fprintln(c.out, "note: self-deploy of the controller (ADR-0005 human gate): the previous release keeps serving until the readiness gate passes; on failure the old release stays in place — re-run `topctl install` to converge it")
				}
			}
			rev := flagString(cmd, "revision")
			if rev != "" {
				if err := domain.ValidRevision(rev); err != nil {
					return c.usageErr(cmd, "--revision: "+err.Error())
				}
			}
			ctx := cmd.Context()
			st, err := c.orStore(ctx)
			if err != nil {
				return c.storeExit(cmd, err)
			}
			d, err := c.resolveDesired(ctx, rev)
			if err != nil {
				return c.resolveExit(cmd, err)
			}
			defer func() { _ = d.snap.Close() }()
			in := c.runInput(d, false, false)
			if selfDeploy {
				// The explicit-human self-deploy: this run MAY apply the
				// revision/image change to the adopted controller (the user
				// actor is what validateRun requires).
				in.SelfDeployApproved = true
			}
			in.Specs, err = deploySpecs(d.specs, ref)
			if err != nil {
				return c.usageErr(cmd, err.Error()) // unknown service/dependency: the operator's ask
			}
			if flagBool(cmd, "dry-run") {
				rep, err := reconcile.DiffOnly(ctx, in, dokku.New(c.orRunner()), st)
				if err != nil {
					return c.resolveExit(cmd, err)
				}
				return c.renderDryPlan(cmd, d, rep)
			}
			return c.execReconcile(cmd, d, in, st)
		},
	}
	cmd.Flags().String("revision", "", "deploy a specific git commit sha instead of the resolved desired revision")
	return cmd
}

// newHistoryCmd — REAL (T-309): docs/contracts/cli.md history row —
// "topctl history [ns/name] [--limit N] | read-only | Store | operation
// records". Reads the append-only operation ledger (newest first).
// Behavior with absent state (a fresh install): an honest empty listing,
// exit 0. Failed rows render with the actionable hint vocabulary the
// T-308 logs command established (readinessHints in stubs.go) handed at
// the history half: a failed row whose redacted error names "ns/name"
// points at `topctl logs <ns/name>` and the last-known-good fact.
func newHistoryCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history [ns/name]",
		Short: "Operation records (deploys, reconciles), newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ref domain.ServiceRef
			if len(args) == 1 {
				var err error
				if ref, err = domain.ParseServiceRef(args[0]); err != nil {
					return c.usageErr(cmd, err.Error())
				}
			}
			limit := flagInt(cmd, "limit")
			if limit < 1 {
				return c.usageErr(cmd, fmt.Sprintf("--limit %d: must be at least 1", limit))
			}
			ctx := cmd.Context()
			st, err := c.orStore(ctx)
			if err != nil {
				return c.storeExit(cmd, err)
			}
			ops, err := historyRows(ctx, st, limit, ref)
			if err != nil {
				return c.storeExit(cmd, err)
			}
			if c.format == FormatJSON {
				return c.emitOk(cmd, fmt.Sprintf("%d operation(s)%s", len(ops), historyFilterNote(ref)), historyData(ops))
			}
			if err := renderHistoryText(c.out, ops); err != nil {
				return &ExitError{Code: ExitFailed, Err: err}
			}
			return nil
		},
	}
	cmd.Flags().Int("limit", 20, "maximum number of operations to print")
	return cmd
}

// historyFilterNote annotates the JSON envelope message when a service
// filter narrowed the listing (traceability, mirror of the text rows).
func historyFilterNote(ref domain.ServiceRef) string {
	if ref.IsZero() {
		return ""
	}
	return " for " + ref.String()
}

// newLogsCmd — REAL (T-308): read-only log surface. Semantics per the
// docs/contracts/cli.md row ("thin, redacted pass-through"): the default
// mode tails the service's recent runtime logs through the backend (the
// T-308 `ServiceLogs` adapter addition); `--failed` shows the last failed
// deploy's output (contract `FailedLogs`). Redaction is exec-layer — the
// CLI adds no sinks (ADR-0008). On top of the requested log the command
// composes `backend.Readiness`: when the service is NOT ready per the
// reconciler gate (deployed && running && last build succeeded && checks
// passing), action hints AND a capped failed-log excerpt ride the output
// ("hints" / "failedLogs" — T-308's actionable-diagnostics compose half;
// the history-entry half is T-309). Exit codes: retrieval ok → 0
// (even with empty output); missing service → "not-found", 3; dokku
// refusing → "failed", 3; backend itself unusable → "backend-unavailable",
// 4. Text mode prints log lines verbatim for humans; -o json emits the
// versioned envelope (doctor pattern).
func newLogsCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <ns/name>",
		Short: "Recent logs for one service (thin, redacted pass-through)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := domain.ParseServiceRef(args[0])
			if err != nil {
				return c.usageErr(cmd, err.Error())
			}
			lines := flagInt(cmd, "lines")
			if lines < 1 {
				return c.usageErr(cmd, fmt.Sprintf("--lines %d: must be at least 1", lines))
			}
			be := dokku.New(c.orRunner())
			var logs []string
			if flagBool(cmd, "failed") {
				logs, err = be.FailedLogs(cmd.Context(), ref, lines)
			} else {
				logs, err = be.ServiceLogs(cmd.Context(), ref, lines)
			}
			if err != nil {
				return c.logsExit(cmd, err)
			}
			res := logsResult{Ref: ref.String(), Lines: logs}
			res.hintsFor(c, cmd.Context(), be, ref, lines)
			if c.format == FormatJSON {
				data := map[string]any{
					"ref":   res.Ref,
					"lines": nonNil(res.Lines),
					"hints": nonNil(res.Hints),
				}
				if res.Readiness != nil {
					data["readiness"] = map[string]any{
						"deployed": res.Readiness.Deployed,
						"running":  res.Readiness.Running,
						"build":    string(res.Readiness.Build),
						"checks":   res.Readiness.Checks,
					}
				}
				if len(res.FailedLogs) > 0 {
					data["failedLogs"] = res.FailedLogs
				}
				return c.emitOk(cmd, fmt.Sprintf("%d log line(s) for %s", len(res.Lines), res.Ref), data)
			}
			res.renderText(c.out)
			return nil
		},
	}
	cmd.Flags().Bool("failed", false, "show the last failed deploy's output instead of recent runtime logs")
	cmd.Flags().Int("lines", 100, "maximum number of log lines to show (most recent kept)")
	return cmd
}

// logsResult is one logs invocation's composed payload: the requested
// lines plus the readiness cues (T-308).
type logsResult struct {
	Ref        string
	Lines      []string
	FailedLogs []string
	Hints      []string
	// Readiness is nil when the readiness composition was unavailable
	// (its failure degraded to a hint).
	Readiness *backend.Readiness
}

// hintsFor composes the readiness cues onto r: a not-ready service yields
// the failure hints plus the capped failed-log excerpt (a missing app
// cannot reach here — the earlier log call already surfaced not-found). A
// Readiness/FailedLogs failure DEGRADES to a hint: the requested log
// retrieved fine and a read-only command must not lose it to a probe
// failure.
func (r *logsResult) hintsFor(c *cctx, ctx context.Context, be backend.PlatformBackend, ref domain.ServiceRef, lines int) {
	rd, err := be.Readiness(ctx, ref)
	if err != nil {
		r.Hints = append(r.Hints, "readiness unavailable: "+err.Error())
		return
	}
	r.Readiness = &rd
	if rd.Deployed && rd.Running && rd.Build == domain.BuildSucceeded && rd.Checks {
		return // ready — no hints, no excerpt
	}
	r.Hints = append(r.Hints, readinessHints(ref.String(), rd)...)
	fl, ferr := be.FailedLogs(ctx, ref, lines)
	switch {
	case ferr == nil:
		r.FailedLogs = fl
	case !errors.Is(ferr, domain.ErrNotFound):
		r.Hints = append(r.Hints, "failed-log excerpt unavailable: "+ferr.Error())
	}
}

// readinessHints renders one hint per broken Readiness facet, ordered as
// the fields of backend.Readiness (the doctor vocabulary's style:
// finding + actionable command). Only the FIRST blocking gate keeps its
// hint when deploy/running cascade (an undeployed app cannot be running).
func readinessHints(ref string, rd backend.Readiness) []string {
	var hints []string
	if !rd.Deployed {
		hints = append(hints, "service is not deployed; run `topctl reconcile` to converge it")
	} else if !rd.Running {
		hints = append(hints, "processes are not running; run `topctl reconcile` (or `dokku ps:restart` for the app directly)")
	}
	switch rd.Build {
	case domain.BuildFailed:
		hints = append(hints, "the last deploy build failed — see the failed-log excerpt (full output: `topctl logs "+ref+" --failed`)")
	case domain.BuildQueued, domain.BuildRunning:
		hints = append(hints, "a deploy build is still in flight; re-run `topctl logs` once it finishes")
	}
	if rd.Deployed && rd.Running && !rd.Checks {
		hints = append(hints, "processes run but health checks fail — inspect the spec's checks (`topctl diff` / reconciler plan)")
	}
	return hints
}

// renderText prints the logs verbatim with the readiness cues trailing
// (read-only, human-first; -o json carries the same facts in the envelope).
func (r *logsResult) renderText(w io.Writer) {
	_, _ = fmt.Fprintf(w, "ref: %s\n", r.Ref)
	if r.Readiness != nil {
		rd := *r.Readiness
		_, _ = fmt.Fprintf(w, "readiness: deployed=%v running=%v build=%s checks=%v\n",
			rd.Deployed, rd.Running, rd.Build, rd.Checks)
	}
	for _, l := range r.Lines {
		_, _ = fmt.Fprintln(w, l)
	}
	if len(r.FailedLogs) > 0 {
		_, _ = fmt.Fprintln(w, "! failed-deploy excerpt (most recent last):")
		for _, l := range r.FailedLogs {
			_, _ = fmt.Fprintln(w, "  "+l)
		}
	}
	for _, h := range r.Hints {
		_, _ = fmt.Fprintln(w, "hint: "+h)
	}
}

// logsExit maps the log-retrieval failure onto the exit table: a missing
// service is "not-found" (3 — a well-formed ask whose object is absent;
// the table's 1 stays reserved for drift/diagnose findings); a dokku
// exit status is "failed" (3); anything else (binary absent, timeout,
// I/O) is the backend itself being unusable: "backend-unavailable" (4).
func (c *cctx) logsExit(cmd *cobra.Command, err error) error {
	var nf domain.NotFoundError
	if errors.As(err, &nf) {
		return c.emitErrCode(cmd, "not-found", ExitFailed, nf.Error())
	}
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		return c.emitErrCode(cmd, "failed", ExitFailed, err.Error())
	}
	return c.emitErrCode(cmd, "backend-unavailable", ExitUnreachable, err.Error())
}

// nonNil keeps empty slices as [] in the JSON envelope (never null: agents
// parse the shape once).
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// newUninstallCmd — REAL (T-208): the ownership-safe teardown behind the
// same flag surface. Each purge tier keeps its own flag (ADR-0009: no
// yes-to-everything); --confirm is the whole command's gate. The fresh
// snapshot + two-key law decide every removal; defaults keep all data.
// Exit codes: nothing-installed / decision findings → 1; failures → 3;
// store unavailability → 4.
func newUninstallCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove TOP's apps and controller from this host (destructive, confirm-gated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.requireConfirm(cmd, "uninstalling TOP (destructive)"); err != nil {
				return err
			}
			runner := c.orRunner()
			opts := install.UninstallOptions{
				Runner:  runner,
				Backend: dokku.New(runner),
				DataDir: c.cfg.DataDir,

				PurgeData:      flagBool(cmd, "purge-data"),
				PurgeSecrets:   flagBool(cmd, "purge-secrets"),
				UninstallDokku: flagBool(cmd, "uninstall-dokku"),

				Confirm: true, // requireConfirm above IS the gate
				DryRun:  flagBool(cmd, "dry-run"),
			}
			res, err := install.Uninstall(cmd.Context(), opts)
			if err != nil {
				return c.installExit(cmd, res, err)
			}
			return c.emitInstallResult(cmd, res)
		},
	}
	cmd.Flags().Bool("purge-data", false, "also delete TOP's state under <dataDir> (the SQLite file + any other state files; secrets/git keys untouched — their own tier)")
	cmd.Flags().Bool("purge-secrets", false, "also delete the secret files and git key material under <dataDir> (irreversible)")
	cmd.Flags().Bool("uninstall-dokku", false, "also remove Dokku itself (separate tier; prints what its uninstaller leaves behind)")
	return cmd
}

// newControllerCmd — the live daemon (T-310): `topctl controller run`
// starts the poll/webhook/sweep producers, the reconcile pump, and the
// HTTP endpoint; graceful shutdown is controller.Serve's contract.
func newControllerCmd(c *cctx) *cobra.Command {
	d := &cobra.Command{
		Use:   "run",
		Short: "Run the reconciliation controller loops and HTTP endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Wave-C composition (internal/controller README): load
			// config → construct the seams → fill → Serve. The durable
			// store and the reconciler belong to the DAEMON lifetime
			// (unlike firstrun's per-run handoff); they close on exit.
			ctx := cmd.Context()
			st, err := sqlite.Open(ctx, c.cfg.DataDir+"/top.db")
			if err != nil {
				return c.storeExit(cmd, err)
			}
			defer func() { _ = st.Close() }()
			rec, err := c.orReconciler(st)
			if err != nil {
				return &ExitError{Code: ExitFailed, Err: err}
			}
			if c.cfg.SecretsProvider.Kind != config.SecretsProviderHostfile {
				return c.emitErrCode(cmd, "failed", ExitFailed,
					fmt.Sprintf("controller: secretsProvider.kind %q is not hostfile (ADR-0008: the webhook-hmac secret file must be provisionable; other kinds are not wired for the daemon yet)", c.cfg.SecretsProvider.Kind))
			}
			o := controller.OptionsFromConfig(c.cfg)
			o.Reconciler = rec
			o.Store = st
			o.Source = c.orGitSource()
			o.Secret = secrets.NewHostFile(c.cfg.SecretsDir())
			if err := controller.Serve(o); err != nil {
				// Serve returns only startup/validation failures
				// (invalid config, missing store component) — a clean
				// SIGINT/SIGTERM shutdown is Serve's nil contract.
				return c.emitErrCode(cmd, "failed", ExitFailed, err.Error())
			}
			return nil
		},
	}
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the TOP reconciliation controller (usually run by Dokku, not by hand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.usageErr(cmd, "controller requires the subcommand `run`")
		},
	}
	cmd.AddCommand(d)
	return cmd
}
