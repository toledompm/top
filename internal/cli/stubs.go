package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/toledompm/top/internal/domain"
)

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

// newDoctorCmd — read-only stub (installer phase 0 in M2).
func newDoctorCmd(c *cctx) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check host preconditions (pre-install environment check)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.notImpl(cmd, false)
		},
	}
}

// newInstallCmd — mutating, confirm-gated stub.
func newInstallCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install <repo>",
		Short: "Install TOP on this host from a platform repo (confirm-gated)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.requireConfirm(cmd, "installing TOP on this host"); err != nil {
				return err
			}
			return c.notImpl(cmd, true)
		},
	}
	addConfirmFlag(cmd)
	cmd.Flags().Bool("install-dokku", false, "also install Dokku if absent")
	cmd.Flags().Bool("dry-run", false, "print the plan and mutate nothing")
	return cmd
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
	addConfirmFlag(cmd)
	cmd.Flags().Bool("now", false, "reconcile immediately instead of honoring locks/backoff")
	cmd.Flags().String("service", "", "reconcile a single service (\"ns/name\"; bare \"name\" = default/name)")
	cmd.Flags().Bool("prune", false, "delete services observed but absent from the spec (confirm-gated)")
	cmd.Flags().Bool("include-controller", false, "include TOP's own controller in the reconcile (confirm-gated, ADR-0005)")
	cmd.Flags().Bool("dry-run", false, "print the plan and mutate nothing")
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
	cmd.Flags().Bool("dry-run", false, "print the plan and mutate nothing")
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
	addConfirmFlag(create)
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

// newRecoverCmd — diagnose (read-only) plus restart/redeploy/restore
// (mutating, gated; these work with the controller down, ADR-0005).
func newRecoverCmd(c *cctx) *cobra.Command {
	recover := &cobra.Command{
		Use:   "recover",
		Short: "Controllerless diagnosis and repair (works with controller down, ADR-0005)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.usageErr(cmd, "recover requires a subcommand: diagnose, restart, redeploy, or restore")
		},
	}

	diagnose := &cobra.Command{
		Use:   "diagnose",
		Short: "Report state of dokku, store, secrets, and controller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.notImpl(cmd, false)
		},
	}
	recover.AddCommand(diagnose)

	mutatingShort := [][2]string{
		{"restart", "Restart the TOP controller without a full deploy (confirm-gated)"},
		{"redeploy", "Force a redeploy of services from the last known-good state (confirm-gated)"},
		{"restore", "Restore TOP's metadata from a backup (confirm-gated)"},
	}
	for _, pair := range mutatingShort {
		verb, short := pair[0], pair[1]
		sub := &cobra.Command{
			Use:   verb,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if err := c.requireConfirm(cmd, "recover "+verb+" on this host"); err != nil {
					return err
				}
				return c.notImpl(cmd, true)
			},
		}
		addConfirmFlag(sub)
		sub.Flags().Bool("dry-run", false, "print the plan and mutate nothing")
		recover.AddCommand(sub)
	}
	return recover
}

// newUninstallCmd — destructive, gated stub. Tiered per-flag
// confirmations (--purge-data etc., ADR-0009) arrive with the real
// teardown in M2; the stub still requires --confirm for the whole thing.
func newUninstallCmd(c *cctx) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove TOP's apps and controller from this host (destructive, confirm-gated)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := c.requireConfirm(cmd, "uninstalling TOP (destructive)"); err != nil {
				return err
			}
			return c.notImpl(cmd, true)
		},
	}
	addConfirmFlag(cmd)
	cmd.Flags().Bool("purge-data", false, "also delete /var/lib/top state (irreversible)")
	cmd.Flags().Bool("purge-secrets", false, "also delete the secret store (irreversible)")
	cmd.Flags().Bool("uninstall-dokku", false, "also uninstall Dokku itself")
	cmd.Flags().Bool("dry-run", false, "print the plan and mutate nothing")
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
