// Package exec is TOP's safe subprocess wrapper. All host mutation and every
// external command TOP runs (dokku, git, install helpers) goes through here.
//
// Invariants enforced (ADR-0006):
//
//   - argv slices only. There is no API accepting a command string, so
//     shell-string construction is structurally impossible. Callers pass a
//     binary name plus arguments; quoting/escaping is never needed.
//   - no identifier validation lives here. name/arg validation belongs to the
//     callers (backend/dokku later). The only structural refusals are an
//     empty binary name and any argument containing a NUL byte (NUL acts as
//     the argv separator and would silently truncate an argument).
//   - caller-controlled per-call deadlines via ctx; on deadline the child is
//     asked to terminate (SIGTERM via cmd.Cancel, never assuming preset
//     escalation) and, after WaitDelay, is SIGKILLed by os/exec. The returned
//     error is a typed timeout wrapping context.Canceled.
//   - env hygiene: by default the child gets a minimal, explicit environment
//     (DefaultEnvPassthrough plus ExtraEnv), never the parent env wholesale.
//     EnvInheritAll is the opt-in escape hatch for bespoke CLI use.
//   - redaction: registered secret values (RedactSeed) are replaced with
//     "***" in both captured buffers before they are returned and inside
//     every error string this package produces (ADR-0008: no secret value
//     ever reaches logs, operation records, or CLI output — only names).
//
// The Runner interface is the seam used everywhere else;
// internal/exec/exectest provides the test double.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"strings"
	"syscall"
	"time"
)

// DefaultEnvPassthrough is the minimal set of environment variable names
// passed through from the parent environment for every FastExec run.
// Everything else the parent has is not forwarded unless the caller adds it
// via EnvPassthrough/ExtraEnv or opts into EnvInheritAll.
var DefaultEnvPassthrough = []string{"PATH", "HOME", "LANG", "TMPDIR"}

// DefaultWaitDelay is how long a cancelled child gets between SIGTERM and the
// SIGKILL that os/exec applies at the WaitDelay bound. Short on purpose: a
// hung child must never stall reconciliation for long.
const DefaultWaitDelay = 5 * time.Second

// ErrTimeout wraps context.Canceled when a per-call deadline fired. Match
// with errors.Is(err, ErrTimeout); the underlying context error stays
// reachable through Unwrap for diagnostics.
var ErrTimeout = errors.New("exec: timed out")

// ErrNotFound reports that the binary could not be resolved in PATH.
var ErrNotFound = errors.New("exec: binary not found")

// ExcerptLimit bounds the redacted stderr excerpt embedded in ExitError text.
const ExcerptLimit = 200

// Runner is the only way TOP invokes external processes. stdout and stderr
// are captured separately; err is nil for exit status 0 even when stderr is
// non-empty — a non-empty stderr is a warning the caller may surface by
// checking the buffer itself; exit status, not stderr, decides success.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

	// RunStream streams stdout/stderr into the given writers instead of
	// buffering. Reserved for long-running deploy paths. Redaction cannot be
	// applied to a live stream without corrupting it, so streaming callers
	// must only sink commands whose output is not secret-bearing, or must
	// not forward the stream verbatim.
	RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
}

// StdinRunner is the optional, ADDITIVE capability seam for commands that
// must receive their primary input on STDIN (ADR-0002's synthesized
// healthcheck archive: `git:from-archive <app> --`). Keep it a separate
// interface — extended, never forked — so Runners that cannot feed stdin
// stay valid implementations of Runner. Redaction rules are identical to
// Run: captured buffers are redacted from the seed before return.
type StdinRunner interface {
	// RunStdin runs the command with stdin fed from the given reader.
	// Captures stdout/stderr exactly like Run and returns a typed
	// *ExitError / ErrTimeout / ErrNotFound under the same rules.
	RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) (stdout, stderr []byte, err error)
}

// FastExec is the production Runner. The zero value is usable: minimal env
// passthrough, DefaultWaitDelay, no redaction seed.
type FastExec struct {
	// Dir is the child's working directory; empty means inherit.
	Dir string

	// EnvPassthrough names additional parent env vars to copy into the child,
	// on top of DefaultEnvPassthrough. Ignored under EnvInheritAll.
	EnvPassthrough []string

	// ExtraEnv are full KEY=VALUE pairs set on the child verbatim, always
	// (also under EnvInheritAll). Secret values passed here must also be
	// registered in RedactSeed so any echo of them never leaves unredacted.
	ExtraEnv []string

	// EnvInheritAll gives the child this process' whole environment plus
	// ExtraEnv. Opt-in only, for bespoke CLI use — never the
	// reconciler's steady-state path.
	EnvInheritAll bool

	// RedactSeed registers secret *values*. Every occurrence in captured
	// stdout/stderr and in any error string produced here is replaced with
	// "***" before anything leaves this package. Register the value, not the
	// name (names may appear anywhere).
	RedactSeed []string

	// WaitDelay bounds how long a cancelled child has between SIGTERM and
	// SIGKILL. Zero means DefaultWaitDelay.
	WaitDelay time.Duration
}

var _ Runner = (*FastExec)(nil)

// NewFastExec returns a Runner with defaults applied. Use the struct directly
// when you need Dir/extra env/seed configuration.
func NewFastExec() *FastExec { return &FastExec{} }

// Run implements Runner.
func (f *FastExec) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	return f.run(ctx, nil, name, args...)
}

// RunStdin implements StdinRunner: identical to Run, with cmd.Stdin wired to
// stdin. The child always gets stdin here (nil falls back to /dev/null
// behavior of cmd.Stdin == nil, so a nil reader is reserved for callers that
// genuinely want no stdin pipe).
func (f *FastExec) RunStdin(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, error) {
	return f.run(ctx, stdin, name, args...)
}

// run is the shared Run/RunStdin body; streaming variants are separate
// because they have different buffer semantics.
func (f *FastExec) run(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, error) {
	if err := validate(name, args); err != nil {
		return nil, nil, err
	}
	cmd, out, errOut := f.newCmd(ctx, nil, nil, name, args)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if err := cmd.Start(); err != nil {
		return nil, errOut.Bytes(), f.startErr(err, name)
	}
	err := f.wait(ctx, cmd, out, errOut)
	return f.redact(out.Bytes()), f.redact(errOut.Bytes()), f.redactErr(err)
}

// RunStream implements Runner. See the interface doc for the redaction caveat.
func (f *FastExec) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	if err := validate(name, args); err != nil {
		return err
	}
	cmd, _, _ := f.newCmd(ctx, stdout, stderr, name, args)
	if err := cmd.Start(); err != nil {
		return f.redactErr(f.startErr(err, name))
	}
	return f.redactErr(f.wait(ctx, cmd, nil, nil))
}

// startErr classifies Start failures, primarily the missing-binary case.
func (f *FastExec) startErr(err error, name string) error {
	var osexecErr *osexec.Error
	if errors.As(err, &osexecErr) && osexecErr.Err == osexec.ErrNotFound {
		return fmt.Errorf("run %s: %w: no such binary", name, ErrNotFound)
	}
	return fmt.Errorf("run %s: %w", name, err)
}

func (f *FastExec) newCmd(ctx context.Context, stdout, stderr io.Writer, name string, args []string) (*osexec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	cmd := osexec.CommandContext(ctx, name, args...) //nolint:gosec // argv slices only; no shell string is ever constructed by this package
	cmd.Dir = f.Dir
	cmd.Env = f.childEnv()
	cmd.WaitDelay = f.waitDelayOrDefault()
	cmd.Cancel = func() error {
		// First ask politely. If the child ignores SIGTERM, os/exec kills it
		// once WaitDelay elapses (SIGTERM → SIGKILL escalation handled by
		// os/exec itself). Must be set before Start (race with os/exec's
		// internal watch goroutine otherwise).
		_ = cmd.Process.Signal(syscall.SIGTERM)
		return nil
	}
	var out, errOut bytes.Buffer
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = &out
	}
	if stderr != nil {
		cmd.Stderr = stderr
	} else {
		cmd.Stderr = &errOut
	}
	return cmd, &out, &errOut
}

// wait performs the Wait + error classification (Cancel/WaitDelay are set in
// newCmd, before Start).
func (f *FastExec) wait(ctx context.Context, cmd *osexec.Cmd, out, errOut *bytes.Buffer) error {
	waitErr := cmd.Wait()
	name := shortName(cmd.Path)
	// Timeout/cancel classification comes first: once the context is dead the
	// run is over from the caller's point of view, whatever the child did.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("run %s: %w: %w", name, ErrTimeout, ctxErr)
	}
	if waitErr != nil {
		var exitErr *osexec.ExitError
		if errors.As(waitErr, &exitErr) {
			se := f.stderrOf(exitErr, errOut)
			e := &ExitError{Err: exitErr, Name: name, Args: cmd.Args[1:], ExitCode: exitErr.ExitCode(), Stderr: se}
			if se == nil {
				e.Stderr = nil
			}
			return e
		}
		return fmt.Errorf("run %s: %w", name, waitErr)
	}
	return nil
}

func (f *FastExec) stderrOf(exitErr *osexec.ExitError, errOut *bytes.Buffer) []byte {
	// We always set cmd.Stderr to our own capture, so os/exec does not attach
	// its buffer to exitErr.Stderr; use ours. Precedence still goes to
	// os/exec's own field in exotic streaming cases where it is populated.
	if b := exitErr.Stderr; len(b) > 0 {
		return b
	}
	if errOut != nil && errOut.Len() > 0 {
		return errOut.Bytes()
	}
	return nil
}

func (f *FastExec) waitDelayOrDefault() time.Duration {
	if f.WaitDelay > 0 {
		return f.WaitDelay
	}
	return DefaultWaitDelay
}

// shortName strips a resolved binary path down to a readable label.
func shortName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// childEnv builds the child environment per the hygiene rules.
func (f *FastExec) childEnv() []string {
	if f.EnvInheritAll {
		return append(append([]string{}, os.Environ()...), f.ExtraEnv...)
	}
	passthrough := make(map[string]bool, len(DefaultEnvPassthrough)+len(f.EnvPassthrough))
	for _, k := range DefaultEnvPassthrough {
		passthrough[k] = true
	}
	for _, k := range f.EnvPassthrough {
		passthrough[k] = true
	}
	env := make([]string, 0, len(passthrough)+len(f.ExtraEnv))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 && passthrough[kv[:i]] {
			env = append(env, kv)
		}
	}
	return append(env, f.ExtraEnv...)
}

// redact replaces secret values in captured output.
func (f *FastExec) redact(b []byte) []byte { return Redact(b, f.RedactSeed) }

// redactErr applies redaction to an error before it may surface in operation
// records or logs. Sentinel identity (ErrTimeout) is preserved; other chain
// detail is folded into the redacted rendering so no seed can leak through a
// later Unwrap.
func (f *FastExec) redactErr(err error) error {
	if err == nil || len(f.RedactSeed) == 0 {
		return err
	}
	if errors.Is(err, ErrTimeout) {
		return fmt.Errorf("%s: %w", RedactString(err.Error(), f.RedactSeed), ErrTimeout)
	}
	// Preserve ExitError identity so callers' errors.As checks keep working;
	// redact any seed that may sit in argv.
	var ee *ExitError
	if errors.As(err, &ee) {
		r := *ee
		for i, a := range r.Args {
			r.Args[i] = RedactString(a, f.RedactSeed)
		}
		r.Stderr = f.redact(ee.Stderr)
		return &r
	}
	return errors.New(RedactString(err.Error(), f.RedactSeed))
}

// Redact replaces every occurrence of each seed value in data with "***".
// Empty seeds are ignored (they would redact everything).
func Redact(data []byte, seeds []string) []byte {
	return []byte(RedactString(string(data), seeds))
}

// RedactString is Redact for strings (error text, messages).
func RedactString(s string, seeds []string) string {
	for _, seed := range seeds {
		if seed != "" {
			s = strings.ReplaceAll(s, seed, redactionMarker)
		}
	}
	return s
}

// redactionMarker is the constant substituted for every secret value.
const redactionMarker = "***"

// validate refuses an empty binary name and NUL bytes anywhere in argv — the
// only structural validation in this package.
func validate(name string, args []string) error {
	if name == "" {
		return errors.New("exec: empty command name")
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("exec: command name contains NUL byte: %q", name)
	}
	for _, a := range args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("exec: argument contains NUL byte: %q", truncateForLog(a))
		}
	}
	return nil
}

func truncateForLog(s string) string {
	s = strings.ReplaceAll(s, "\x00", "\\x00")
	if len(s) > ExcerptLimit {
		s = s[:ExcerptLimit] + "…"
	}
	return s
}
