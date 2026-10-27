package cli

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Confirm scaffolding for approval-gated commands (ADR-0009 and the
// docs/contracts/cli.md safety invariants).
//
// Full enforcement grows with the real mutating operations in M2; this
// is the single knob every gated command calls, so wiring later is a
// one-line change per command:
//
//   - `--confirm` present          → proceed. Deterministic, non-interactive
//     path for agents and CI (ADR-0009: first-class).
//   - no --confirm, stdin is a TTY → interactive y/N prompt.
//   - no --confirm, non-TTY        → usage error (exit 2) with guidance, so a
//     scripted caller never silent-prompts and never silent-approves.
//
// Destructive actions must also carry their specific flag (--purge-data,
// --prune, …): there is no single "yes to everything" switch (ADR-0009).
// In the stub phase the gate runs at flag-validation time only — no
// confirmation artifact is persisted yet; that audit trail is M2 work.

// stdinIsTTY answers "could we ask a human right now?" Tests force the
// answer with Options.StdinIsTTY (real detection otherwise), because a
// captured io.Reader can never be a character device.
func (c *cctx) stdinIsTTY() bool {
	if c.opts.StdinIsTTY != nil {
		return *c.opts.StdinIsTTY
	}
	return isTTY(c.stdin)
}

// requireConfirm enforces the gate for the named action.
//
// Testing note: the interactive branch is driven by Options.Stdin/Stdout
// with StdinIsTTY forced; no PTY/expect harness is needed (see
// internal/cli/confirm_test.go).
func (c *cctx) requireConfirm(cmd *cobra.Command, action string) error {
	if flagBool(cmd, "confirm") {
		return nil
	}
	if c.stdinIsTTY() {
		return c.promptConfirm(cmd, action)
	}
	return c.usageErr(cmd, fmt.Sprintf("%s mutates the host: it requires --confirm to run non-interactively; re-run with --confirm (or from a terminal, where you will be asked)", action))
}

// promptConfirm asks the human at the terminal. An affirmative answer
// proceeds; anything else is a usage failure ("failed to authorize", not
// a crash or a silent skip).
func (c *cctx) promptConfirm(cmd *cobra.Command, action string) error {
	_, _ = fmt.Fprintf(c.out, "topctl %s: %s. Proceed? [y/N] ", invocation(cmd), action)
	answer, _ := bufio.NewReader(c.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return nil
	case "", "n", "no":
		return c.usageErr(cmd, "no confirmation given; operation aborted")
	default:
		return c.usageErr(cmd, fmt.Sprintf("unrecognized answer %q: confirm with --confirm, or answer y/N", answer))
	}
}
