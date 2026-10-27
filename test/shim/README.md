# test/shim — tier-2 shim integration (ADR-0012)

The seam proven here is `exec.Runner` (ADR-0006): a **real** child process —
the fake `dokku` script in `bin/dokku` — is resolved via PATH and invoked by
production `exec.FastExec`, so argv construction, exit-code mapping, stderr
capture, timeout classification and redaction are exercised end-to-end
without Dokku. This is tier 2 of the test pyramid; tier 1 (pure fakes) lives
in `internal/exec/exectest`, tiers 3–4 use real Dokku.

**It is not imported by production code and never touches a real host.**

## Files

- `bin/dokku` — POSIX-sh shim: records argv, serves canned responses, injects failures, refuses malformed usage.
- `run-with-shim.sh` — runs any command with `bin/dokku` first on PATH and the shim env set, cleaning up afterwards.
- `integration_test.go` — the derived loop; **guarded by `//go:build toptests`**, so plain `go test ./...` (and CI) stays hermetic and fast. Run via `make test-shim`.

## Registry layout (`$DOKKU_SHIM_FIXTURES` dir)

Responses are plain files, no JSON parsing in the shell. Precedence, most
specific first:

| When the call is… | Files consulted |
| --- | --- |
| exact argv match | `<sha1("dokku " + space-joined args)>.{stdout,stderr,exit}` |
| same subcommand, any args | `subcmd.<sub>.{stdout,stderr,exit}` |
| same family (first arg up to `:`) | `prefix.<family>.{stdout,stderr,exit}` |
| nothing matched | default: exit 0, empty stdout/stderr |

`.exit` contains a decimal exit code, `.stdout`/`.stderr` the literal byte
streams. A response exists iff its `.exit` file exists — `fakeexec`
installers always write all three.

Quietest shortest possible setup:

```sh
DOKKU_SHIM_FIXTURES=/tmp/stack   # files: <sha1>.stdout/.stderr/.exit
DOKKU_SHIM_LOG=/tmp/log.jsonl    # one {"name","args","malformed"} per line
DOKKU_SHIM_STATE=/tmp/state      # failure-injection counters
PATH=/path/to/top/test/shim/bin:$PATH dokku apps:list --format json
```

## Failure injection (retry-path tests)

```sh
DOKKU_SHIM_FAIL_ONCE='config:set|37|1'   # subcommand|exitcode|nth call
```

The **first** (`|1` is the default nth) invocation whose first arg is
`config:set` prints a one-line reason to stderr and exits 37; every later
invocation resolves fixtures normally. Invocation counts live in
`$DOKKU_SHIM_STATE`. Use `|` as the separator (`:` is Dokku's own subcommand
syntax). Failed injected calls are still logged like any other.

## Malformed-usage recording

Zero args, empty args, newline-bearing or >1024-byte args, or >128 args are
not executed: the log line carries `"malformed":true` while the offending
argv is still recorded, and the shim exits 2. Parse the log and assert no
further invocation was attempted.

## Running

```sh
make test-shim                                   # tagged Go integration test
test/shim/run-with-shim.sh --keep -- dokku version   # poke the shim by hand
```
