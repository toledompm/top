package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/install"
	"github.com/toledompm/top/internal/recover"
)

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

// Every command from the docs/contracts/cli.md table is defined here as a
// *validated stub*: flags parse, arguments and flag values validate
// against internal/domain, config is loaded (root persistentPreRun), and
// the command prints the versioned envelope. No stub touches the store,
// exec layer, or host — see internal/cli/README.md for how M2/M3 replaces
// each body with a service call behind the identical flag surface.

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
// same flag surface the stub validated. Wiring mirrors the doctor/recover
// bodies: streams+config from the cctx, Runner from Options (fake in
// tests), the concrete backend wired HERE (composition root — internal/
// install only knows the PlatformBackend interface, ADR-0006). Exit codes:
// preflight findings → 1 (drift space, doctor's row); gate/sudo refusals
// and phase failures → 3; store unavailability → 4; usage → 2. --dry-run
// prints the plan, mutates nothing (ADR-0009 dry-run requirement).
//
// NOTE (M-scope): repo creation/--init is T-212/M3; deploy-key GENERATION
// is M3 (the operator's paths are required in v0). The interactive TTY is
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
				Runner:          runner,
				Backend:         dokku.New(runner),
				DataDir:         c.cfg.DataDir,
				RepoURL:         args[0],
				Branch:          c.cfg.Git.Branch,
				InstallDokku:    flagBool(cmd, "install-dokku"),
				Confirm:         true, // requireConfirm above IS the gate
				DryRun:          flagBool(cmd, "dry-run"),
				DeployKey:       flagString(cmd, "deploy-key"),
				KnownHosts:      flagString(cmd, "known-hosts"),
				ControllerImage: flagString(cmd, "controller-image"),
				ControllerTag:   flagString(cmd, "controller-tag"),
				Namespaces:      c.cfg.Namespaces.Reserved,
				SkipPorts:       flagBool(cmd, "skip-ports"),
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

// newStatusCmd — read-only stub.
func newStatusCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Per-service revision, state, health, and last operation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ns := flagString(cmd, "namespace"); ns != "" {
				if err := domain.ValidNamespace(ns); err != nil {
					return c.usageErr(cmd, "--namespace: "+err.Error())
				}
			}
			// --watch is accepted but meaningless on a stub; never sleep.
			return c.notImpl(cmd, false)
		},
	}
	cmd.Flags().String("namespace", "", "restrict output to one TOP namespace")
	cmd.Flags().Bool("watch", false, "stream status updates until interrupted")
	return cmd
}

// newDiffCmd — read-only stub (zero side effects even when real, M2).
func newDiffCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Desired vs observed services (zero side effects; exit 1 on drift)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if rev := flagString(cmd, "revision"); rev != "" {
				if err := domain.ValidRevision(rev); err != nil {
					return c.usageErr(cmd, "--revision: "+err.Error())
				}
			}
			return c.notImpl(cmd, false)
		},
	}
	cmd.Flags().String("revision", "", "diff against a specific git commit sha instead of HEAD")
	return cmd
}

// newReconcileCmd — mutating stub. --prune and --include-controller need
// their own confirmation in addition to the capability being real (M2).
func newReconcileCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Run the reconciler across namespaces (or one service)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if svc := flagString(cmd, "service"); svc != "" {
				if _, err := domain.ParseServiceRef(svc); err != nil {
					return c.usageErr(cmd, "--service: "+err.Error())
				}
			}
			if flagBool(cmd, "prune") || flagBool(cmd, "include-controller") {
				if err := c.requireConfirm(cmd, "reconcile --prune / --include-controller deletes or changes workloads"); err != nil {
					return err
				}
			}
			return c.notImpl(cmd, true)
		},
	}
	cmd.Flags().Bool("now", false, "reconcile immediately instead of honoring locks/backoff")
	cmd.Flags().String("service", "", "reconcile a single service (\"ns/name\"; bare \"name\" = default/name)")
	cmd.Flags().Bool("prune", false, "delete services observed but absent from the spec (confirm-gated)")
	cmd.Flags().Bool("include-controller", false, "include TOP's own controller in the reconcile (confirm-gated, ADR-0005)")
	return cmd
}

// newDeployCmd — mutating stub; the ServiceRef argument is validated even
// in the stub (per-task requirement and the honest failure surface).
func newDeployCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <ns/name>",
		Short: "Deploy one service to its desired (or given) revision",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := domain.ParseServiceRef(args[0]); err != nil {
				return c.usageErr(cmd, err.Error())
			}
			if rev := flagString(cmd, "revision"); rev != "" {
				if err := domain.ValidRevision(rev); err != nil {
					return c.usageErr(cmd, "--revision: "+err.Error())
				}
			}
			return c.notImpl(cmd, true)
		},
	}
	cmd.Flags().String("revision", "", "deploy a specific git commit sha instead of the resolved desired revision")
	return cmd
}

// newHistoryCmd — read-only stub.
func newHistoryCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history [ns/name]",
		Short: "Operation records (deploys, reconciles), newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if _, err := domain.ParseServiceRef(args[0]); err != nil {
					return c.usageErr(cmd, err.Error())
				}
			}
			if n := flagInt(cmd, "limit"); n < 1 {
				return c.usageErr(cmd, fmt.Sprintf("--limit %d: must be at least 1", n))
			}
			return c.notImpl(cmd, false)
		},
	}
	cmd.Flags().Int("limit", 20, "maximum number of operations to print")
	return cmd
}

// newLogsCmd — read-only stub (redacted pass-through once real).
func newLogsCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <ns/name>",
		Short: "Stream recent logs for one service (redacted pass-through)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := domain.ParseServiceRef(args[0]); err != nil {
				return c.usageErr(cmd, err.Error())
			}
			return c.notImpl(cmd, false)
		},
	}
	cmd.Flags().Bool("failed", false, "show only pods/deploys that failed")
	return cmd
}

// newBackupCmd — subcommand group: create (mutating, gated), list/verify
// (read-only).
func newBackupCmd(c *cctx) *cobra.Command {
	backup := &cobra.Command{
		Use:   "backup",
		Short: "Take, list, or verify TOP metadata backups (ADR-0007)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.usageErr(cmd, "backup requires a subcommand: create, list, or verify")
		},
	}

	create := &cobra.Command{
		Use:   "create",
		Short: "Take a backup of TOP's metadata (confirm-gated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.requireConfirm(cmd, "taking a backup of TOP's metadata"); err != nil {
				return err
			}
			return c.notImpl(cmd, true)
		},
	}
	create.Flags().Bool("dry-run", false, "print the plan and mutate nothing")

	list := &cobra.Command{
		Use:   "list",
		Short: "List existing backups",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.notImpl(cmd, false)
		},
	}

	verify := &cobra.Command{
		Use:   "verify [label]",
		Short: "Verify a backup's integrity (default: latest)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.notImpl(cmd, false)
		},
	}

	backup.AddCommand(create, list, verify)
	return backup
}

// newRecoverCmd — the independent recovery path (ADR-0005): diagnose
// fully real (M2 slice of docs/contracts/backup-recovery.md's Recovery
// section); restart/redeploy real-but-minimal; restore scaffold-only
// (lands M5/T-503 behind the ADR-0007 safeguards). All broadcast through
// internal/recover with Options.Runner as the process seam — the real
// host unless a test injected a fake. Findings render in the doctor
// vocabulary (internal/doctor's Report shape, reused); exit code:
// diagnose fails → 1 (the contract's "drift/diagnose-findings" space),
// failed restart/redeploy/restore → 3 (operation failed), store
// unavailability during a mutation → 4.
func newRecoverCmd(c *cctx) *cobra.Command {
	recoverCmd := &cobra.Command{
		Use:   "recover",
		Short: "Controllerless diagnosis and repair (works with controller down, ADR-0005)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.usageErr(cmd, "recover requires a subcommand: diagnose, restart, redeploy, or restore")
		},
	}

	diagnose := &cobra.Command{
		Use:   "diagnose",
		Short: "Report state of dokku, store, secrets, and controller (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := recover.Diagnose(cmd.Context(), c.recoverOpts())
			if c.format == FormatJSON {
				if err := c.emitOk(cmd, recover.Summary(rep), rep.EnvelopeData()); err != nil {
					return err
				}
			} else if err := recover.RenderText(c.out, rep); err != nil {
				return &ExitError{Code: ExitFailed, Err: err}
			}
			if !rep.OK() {
				return &ExitError{
					Code: ExitDrift,
					Err:  errors.New("recover diagnose: failures found (see per-finding suggested commands above)"),
				}
			}
			return nil
		},
	}
	recoverCmd.AddCommand(diagnose)

	restart := &cobra.Command{
		Use:   "restart",
		Short: "Restart the TOP controller without a full deploy (confirm-gated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.requireConfirm(cmd, "restarting the TOP controller"); err != nil {
				return err
			}
			return c.runRecoverAction(cmd, "restart", func(o recover.Options) (*recover.Result, error) {
				return recover.Restart(cmd.Context(), o, flagBool(cmd, "dry-run"))
			})
		},
	}
	restart.Flags().Bool("dry-run", false, "print the plan and mutate nothing")

	redeploy := &cobra.Command{
		Use:   "redeploy",
		Short: "Redeploy the controller from last-known-good or the pinned image (confirm-gated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if rev := flagString(cmd, "revision"); rev != "" {
				if err := domain.ValidRevision(rev); err != nil {
					return c.usageErr(cmd, "--revision: "+err.Error())
				}
			}
			if err := c.requireConfirm(cmd, "redeploying the TOP controller via Dokku directly"); err != nil {
				return err
			}
			return c.runRecoverAction(cmd, "redeploy", func(o recover.Options) (*recover.Result, error) {
				return recover.Redeploy(cmd.Context(), o, flagString(cmd, "revision"), flagBool(cmd, "dry-run"))
			})
		},
	}
	redeploy.Flags().String("revision", "", "redeploy a specific git commit sha instead of the store's last-known-good (or the pinned bootstrap image)")
	redeploy.Flags().Bool("dry-run", false, "print the plan and mutate nothing")

	restore := &cobra.Command{
		Use:   "restore",
		Short: "Restore TOP's metadata from a backup (confirm-gated; lands M5)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.requireConfirm(cmd, "restoring TOP's metadata from a backup"); err != nil {
				return err
			}
			if err := recover.Restore(cmd.Context(), c.recoverOpts(), flagString(cmd, "backup")); err != nil {
				if errors.Is(err, recover.ErrRestoreDeferred) {
					return c.emitErrCode(cmd, "not-implemented", ExitFailed, err.Error())
				}
				return c.emitErrCode(cmd, "failed", ExitFailed, err.Error())
			}
			return nil
		},
	}
	restore.Flags().String("backup", "", "backup file to restore (required once restore lands in M5)")
	restore.Flags().Bool("dry-run", false, "print the plan and mutate nothing")

	recoverCmd.AddCommand(restart, redeploy, restore)
	return recoverCmd
}

// runRecoverAction renders a restart/redeploy result (success envelope
// carrying the runbook data, or a failed error envelope with the exact
// retry command).
func (c *cctx) runRecoverAction(cmd *cobra.Command, verb string, do func(recover.Options) (*recover.Result, error)) error {
	res, err := do(c.recoverOpts())
	if err != nil {
		return c.emitErrCode(cmd, "failed", ExitFailed, fmt.Sprintf("%s: %v (touches only the Dokku CLI, the filesystem, and the state file — the same independence as diagnose)", verb, err))
	}
	msg := fmt.Sprintf("%s %s: done via %s", verb, res.App, res.Source)
	if c.format == FormatJSON {
		return c.emitOk(cmd, msg, res.DataMap())
	}
	if err := recover.RenderResultText(c.out, verb, res); err != nil {
		return &ExitError{Code: ExitFailed, Err: err}
	}
	return nil
}

// recoverOpts builds internal/recover.Options from the validated config
// + streams context (the doctor wiring's mirror image).
func (c *cctx) recoverOpts() recover.Options {
	return recover.Options{
		Runner:     c.runner,
		DataDir:    c.cfg.DataDir,
		GitURL:     c.cfg.Git.URL,
		GitBranch:  c.cfg.Git.Branch,
		ConfigFile: c.cfgPathOrCanonical(),
	}
}

// cfgPathOrCanonical resolves the config file diagnose validates: the
// operator's --config path when given, else the canonical
// <dataDir>/top-config.yaml (internal/config README's load order).
func (c *cctx) cfgPathOrCanonical() string {
	if c.cfgPath != "" {
		return c.cfgPath
	}
	return c.cfg.DataDir + "/top-config.yaml"
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
	cmd.Flags().Bool("purge-data", false, "also delete TOP's SQLite state, storage entry, and backups under <dataDir> (irreversible; secrets/git keys untouched — their own tier)")
	cmd.Flags().Bool("purge-secrets", false, "also delete the secret files and git key material under <dataDir> (irreversible)")
	cmd.Flags().Bool("uninstall-dokku", false, "also remove Dokku itself (separate tier; prints what its uninstaller leaves behind)")
	return cmd
}

// newControllerCmd — daemon stub.
func newControllerCmd(c *cctx) *cobra.Command {
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the reconciliation controller loops and HTTP endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.notImpl(cmd, true)
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
	cmd.AddCommand(run)
	return cmd
}
