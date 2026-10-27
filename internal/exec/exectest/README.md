# internal/exec/exectest

The test double for `internal/exec`'s `Runner` — THE seam every adapter task
(`backend/dokku`, `gitsource`, `install`, `recover`) uses to test command
behavior without spawning real processes. See `../README.md` for the
production side and the invariants (argv-only, redaction, env hygiene).

## API

```go
f := exectest.NewFakeRunner()          // satisfies exec.Runner
f.SetOutput("dokku", exectest.Response{Stderr: []byte("no such app"), ExitCode: 1},
    "apps:inspect", "my-app")
f.SetPrefix([]string{"git", "fetch"}, exectest.Response{ExitCode: 0})
f.SetDefault(exectest.Response{Err: errors.New("unexpected command")})
defer f.Reset()                        // reusable between cases

// act — hand f wherever a exec.Runner is expected — then assert:
f.RecordedCommands() // ["dokku apps:inspect my-app", ...]
c, ok := f.Last()
```

- `SetOutput(resp, name, args...)`: match when the call's name and all listed
  args match in order (trailing args allowed). Args up to the response.
- `SetPrefix(prefix, resp)`: match any call whose name+args begin with the
  ordered prefix (last registration wins over earlier ones and exact match).
- `Response{Stdout, Stderr []byte, ExitCode int, Err error}`: `ExitCode != 0`
  yields a real `*exec.ExitError` (so `exec.IsExitCode` works); `Err` is
  returned verbatim for non-exit failure modes (missing binary, deadline).
- `Reset()`: clears canned responses and the call log.

Thread-safe; safe under `-race` with concurrent callers.
