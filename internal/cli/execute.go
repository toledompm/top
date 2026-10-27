package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/exec"
)

// Execute builds the topctl root command, runs argv, and maps the result
// to the contract exit code (docs/contracts/cli.md): an ExitError keeps
// its own code; any other error from cobra is a flag/usage error → 2.
// args==nil means "use os.Args leftovers" (real binary path).
func Execute(opts Options, args []string) int {
	root := NewRoot(opts)
	if opts.Out != nil {
		root.SetOut(opts.Out)
	}
	if opts.Err != nil {
		root.SetErr(opts.Err)
	}
	if opts.Stdin != nil {
		root.SetIn(opts.Stdin)
	}
	if args != nil {
		root.SetArgs(args)
	}
	if err := root.Execute(); err != nil {
		var ee *ExitError
		if errAs(err, &ee) {
			return ee.Code
		}
		// Cobra flag/command errors (unknown flag, unknown command,
		// bad arg count) arrive without anything printed because we
		// SilenceErrors; print the error plus guidance, exit 2.
		root.PrintErrf("topctl: %s\n", err)
		root.PrintErrf("(hint: run `topctl --help` to list commands, or `topctl <command> --help` for details)\n")
		return ExitUsage
	}
	return ExitOK
}

// cctx is the per-run context handed to every command closure: resolved
// streams, output format, the validated config from internal/config,
// and the process runner (Options.Runner or exec.NewFastExec()).
type cctx struct {
	opts      Options
	out, errW io.Writer
	stdin     io.Reader
	cfg       config.Config
	cfgPath   string
	format    Format
	runner    exec.Runner
}

// newContext seeds the per-run context from Options (nil → std streams).
func newContext(opts Options) *cctx {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	errW := opts.Err
	if errW == nil {
		errW = os.Stderr
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	return &cctx{opts: opts, out: out, errW: errW, stdin: stdin, runner: opts.Runner}
}

// emitOk renders a success envelope and returns nil (exit 0).
func (c *cctx) emitOk(cmd *cobra.Command, msg string, data map[string]any) error {
	env := Envelope{
		SchemaVersion: JSONSchemaVersion,
		Command:       invocation(cmd),
		Message:       msg,
		Data:          data,
	}
	if err := RenderEnvelope(c.out, c.format, env); err != nil {
		return &ExitError{Code: ExitFailed, Err: err}
	}
	return nil
}

// notImpl renders the honest-stub response. Read-only stubs exit 0: they
// truthfully did no work and nothing failed. Mutating stubs exit 3
// ("operation failed" in the contract — the operation cannot succeed
// today), never 2: the invocation was well-formed; the gap is in the
// tool. See internal/cli/README.md for the rationale.
func (c *cctx) notImpl(cmd *cobra.Command, mutating bool) error {
	if mutating {
		if err := c.emitErrCode(cmd, "not-implemented", ExitFailed,
			notImplementedOp+"; this command mutates the host and gets real backing in M2/M3"); err != nil {
			return err
		}
		return &ExitError{Code: ExitFailed}
	}
	return c.emitOk(cmd, notImplementedOp, nil)
}

// usageErr renders a validation failure: error envelope on stderr, code
// "usage", exit 2, with a help hint so agents can self-correct.
func (c *cctx) usageErr(cmd *cobra.Command, msg string) error {
	return c.emitErrCode(cmd, "usage", ExitUsage,
		fmt.Sprintf("%s (hint: run `topctl %s --help` for usage)", msg, invocation(cmd)))
}

// emitErrCode writes the error envelope and returns a sentinel error the
// caller maps to code. The error path never fails on the format field
// itself: if -o was invalid, RenderErrEnvelope is done in text mode so
// the diagnosis still reaches the caller.
func (c *cctx) emitErrCode(cmd *cobra.Command, code string, exitCode int, msg string) error {
	var env ErrEnvelope
	env.SchemaVersion = JSONSchemaVersion
	env.Command = invocation(cmd)
	env.Error.Code = code
	env.Error.Message = msg
	format := c.format
	if format != FormatJSON {
		format = FormatText
	}
	if err := RenderErrEnvelope(c.errW, format, env); err != nil {
		return &ExitError{Code: ExitUnreachable, Err: fmt.Errorf("writing error output: %v", err)}
	}
	return &ExitError{Code: exitCode, Err: fmt.Errorf("%s: %s", code, msg)}
}

// invocation renders the contract "command" field: "status",
// "backup create", "recover restart" — the command path without the
// binary name, so it survives any future rename of the binary.
func invocation(cmd *cobra.Command) string {
	root := cmd.Root().Name()
	return strings.TrimPrefix(strings.TrimPrefix(cmd.CommandPath(), root), " ")
}
