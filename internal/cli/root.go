package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/toledompm/top/internal/config"
)

// flagString is in execute.go — keep root.go focused on wiring.

// Command constructors live in stubs.go; only the shared root (global
// flags, config load, streams, exit-code plumbing) is defined here.

// flagString fetches a string persistent/local flag value.
func flagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}

// flagBool fetches a bool flag value (false only on programmer error).
func flagBool(cmd *cobra.Command, name string) bool {
	v, _ := cmd.Flags().GetBool(name)
	return v
}

// flagInt fetches an int flag value (0 on programmer error).
func flagInt(cmd *cobra.Command, name string) int {
	v, _ := cmd.Flags().GetInt(name)
	return v
}

// NewRoot assembles the cobra command tree for topctl from the shapes in
// docs/contracts/cli.md. It is pure with respect to the std streams
// (injectable via Options), which is what makes the whole CLI golden-testable.
func NewRoot(opts Options) *cobra.Command {
	c := newContext(opts)

	root := &cobra.Command{
		Use:               "topctl",
		Short:             "TOP control CLI — GitOps reconciliation for Dokku (docs/contracts/cli.md)",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: c.persistentPreRun,
	}

	pf := root.PersistentFlags()
	pf.StringP("output", "o", string(FormatText), "output format: text or json (agents use json)")
	pf.String("config", "", "path to top-config.yaml (default: defaults + TOP_* env)")
	pf.String("redact", "on", "secret redaction mode; reserved: only `on` is accepted")
	// ADR-0009's gate/dry-run vocabulary is a GLOBAL flag surface: declaring
	// them on the root avoids cobra's legacy interspersed-flags trap, in
	// which a subcommand-local flag seen before the subcommand is mistaken
	// for one that takes a value at the root level (that swallowed
	// `install` and turned the repo URL into the command). Per-command
	// GATING is still enforced in each command's body; these are only the
	// flag declarations.
	pf.Bool("confirm", false, "non-interactive approval for gated commands (ADR-0009)")
	pf.Bool("dry-run", false, "print the plan without executing (mutating commands)")
	pf.String("schema-version", fmt.Sprintf("%d", JSONSchemaVersion),
		"output schema negotiation; reserved: only the current schemaVersion is accepted")

	root.AddCommand(
		newVersionCmd(c),
		newDoctorCmd(c),
		newInstallCmd(c),
		newStatusCmd(c),
		newDiffCmd(c),
		newReconcileCmd(c),
		newDeployCmd(c),
		newHistoryCmd(c),
		newLogsCmd(c),
		newUninstallCmd(c),
		newControllerCmd(c),
	)
	return root
}

// persistentPreRun validates global flags and loads TOP's runtime config
// via internal/config — even for stubs — so the "wrong flag" and "bad
// config" failure surface is contract-complete before M2 wiring lands.
// Config paths that fail to load are usage errors (exit 2): the user told
// us to read something unreadable, which is not a host-side failure.
func (c *cctx) persistentPreRun(cmd *cobra.Command, _ []string) error {
	switch Format(flagString(cmd, "output")) {
	case FormatText, FormatJSON:
	default:
		return c.usageErr(cmd, fmt.Sprintf("-o %q: want text or json", flagString(cmd, "output")))
	}
	c.format = Format(flagString(cmd, "output"))

	if v := flagString(cmd, "redact"); v != "on" {
		return c.usageErr(cmd, fmt.Sprintf("--redact only supports `on` in v0 (got %q); redaction is not optional (ADR-0008)", v))
	}
	if v := flagString(cmd, "schema-version"); v != fmt.Sprintf("%d", JSONSchemaVersion) {
		return c.usageErr(cmd, fmt.Sprintf("--schema-version negotiation is reserved; only %d is supported (got %q)", JSONSchemaVersion, v))
	}

	c.cfgPath = flagString(cmd, "config")
	cfg, err := config.Load(c.cfgPath) // "" → defaults + TOP_* env
	if err != nil {
		return c.usageErr(cmd, "loading config: "+err.Error())
	}
	// internal/config README's load order: CLI flags > TOP_* env > file >
	// defaults — the canonical <dataDir>/top-config.yaml auto-loads when
	// --config is absent and the file exists (Load applies TOP_* env ON
	// TOP of the file's values, so env > file holds; tier-4 verified
	// 2026-11-03: diff/status/history on the VM resolve git.url through
	// it instead of re-exporting TOP_GIT_URL in every shell).
	if c.cfgPath == "" {
		if canonical := cfg.DataDir + "/top-config.yaml"; fileReadable(canonical) {
			canonicalCfg, cerr := config.Load(canonical)
			if cerr != nil {
				return c.usageErr(cmd, "loading config: "+cerr.Error())
			}
			cfg = canonicalCfg
		}
	}
	c.cfg = cfg
	return nil
}

// invocation renders the contract "command" field ("status", "reconcile
// diff"): the command path minus the binary name.
// Single definition lives in execute.go; this comment is where a reader
// would first look for it.
// (definition: execute.go invocation())

// fileReadable reports whether path exists and reads (the canonical
// config's presence gate; anything else — a wrong-mode file, an unreadable
// path — surfaces through config.Load's typed errors, not here).
func fileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	return f.Close() == nil
}
