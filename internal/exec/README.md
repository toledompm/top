# internal/exec

TOP's safe subprocess wrapper (ADR-0006): every external command TOP runs —
dokku, git, install helpers, anything that mutates the host — goes through
this package. It enforces timeouts, argv-only invocation, minimal child
environments, and secret redaction.

## API summary

```go
type Runner interface {
    // Run executes argv, capturing stdout/stderr as separate buffers.
    // err is nil for exit 0 even when stderr is non-empty (a non-empty
    // stderr is a warning the caller may surface; exit status decides).
    Run(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

    // RunStream streams stdout/stderr into writers for long-running
    // deploys. No redaction on streams — see interface doc caveat.
    RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
}
```

Production implementation: `FastExec` (fields: `Dir`, `EnvPassthrough`,
`ExtraEnv`, `EnvInheritAll`, `RedactSeed`, `WaitDelay`). Replace `NewFastExec()`
with a configured struct where needed.

### Invariants

- **argv slices only.** No method takes a command string, so
  `fmt.Sprintf`-built shell lines cannot happen. There is deliberately *no*
  identifier validation here: whether a name or argument is "sensible"
  (e.g. `dokku apps:create <name>` grammar) belongs to callers
  (`backend/dokku` and friends). The only structural refusals are an empty
  binary name and an argument containing NUL (`\x00` is the argv separator;
  it would silently truncate the argument).
- **Timeouts are caller-controlled.** Pass a `context.WithTimeout`; on
  deadline the child gets SIGTERM and, `WaitDelay` later (default 5s),
  SIGKILL. The error is a typed timeout — match
  `errors.Is(err, exec.ErrTimeout)`; the `context.Canceled` cause stays
  reachable via `Unwrap`.
- **Env hygiene.** By default the child receives only `PATH`, `HOME`,
  `LANG`, `TMPDIR` (`DefaultEnvPassthrough`) plus anything you name in
  `EnvPassthrough` or set in `ExtraEnv`. The parent environment is never
  inherited wholesale unless you set `EnvInheritAll: true` — an opt-in for
  bespoke CLI use.
- **Redaction.** Register secret *values* in `RedactSeed` (values, never
  names — names may appear anywhere, ADR-0008). Every occurrence in the
  returned stdout/stderr buffers and in any error string this package
  produces is replaced with `***` before it can reach a log, operation
  record, or the CLI. `exec.Redact`/`exec.RedactString` are exported for
  callers applying the same seeds to other sinks.

### Errors

`*exec.ExitError{Err, Name, Args, ExitCode, Stderr}` — `Stderr` is already
redacted; `Error()` renders the canonical one-liner required by
ADR-0009/contracts: `dokku apps:create my-app: exit 1: <redacted excerpt>`.
Helpers: `exec.IsExitCode(err, code)`; `exec.ErrNotFound` (binary missing);
`exec.ErrTimeout` (deadline). All wrap with `%w` so `errors.Is/As` work down
to the original `*os/exec.ExitError` and `context.Canceled`.

### Test double

`internal/exec/exectest` provides `FakeRunner`: a thread-safe, tiny fake
implementing the same `Runner`. Use `SetOutput` (exact name+args match,
trailing args allowed), `SetPrefix` (ordered `name+args` prefix match), or
`SetDefault`; every call is recorded to `Recorded()` / `RecordedCommands()`;
`Reset()` clears everything. Later registrations win.

```go
f := exectest.NewFakeRunner()
f.SetOutput("dokku", tape.Response{}, "apps:create", "my-app")
f.SetPrefix([]string{"git", "fetch"}, exectest.Response{ExitCode: 128})
// use f as the Runner; assert on f.RecordedCommands()
```

## Examples

```go
r := &exec.FastExec{
    RedactSeed: []string{tokenValue},
    ExtraEnv:   []string{"GIT_SSH_COMMAND=" + sshCmd}, // must also be redact-seeded if it embeds the key path only — values, beware
}
stdout, stderr, err := r.Run(context.Background(), "git", "fetch", "--quiet", "origin")
switch {
case err == nil:
case errors.Is(err, exec.ErrTimeout):
case errors.Is(err, exec.ErrNotFound):
    // hint: dokku/git missing on host
}
var ee *exec.ExitError
if errors.As(err, &ee) { /* ee.ExitCode, ee.Stderr */ }
```

## Tests

Test table lives in `internal/exec/exec_test.go`. Real-process tests build
tiny helper binaries (`exectest/cmd/helper-echo`, `helper-env`) in `TestMain`
with a plain `go build` into a temp dir — hermetic and portable across
macOS/Linux. `false`/`sleep` (POSIX everywhere) cover exit codes and
timeouts; the env/refusal/redaction path use the helpers. Never rely on
shell builtins or absolute paths like `/usr/bin/printf`.
