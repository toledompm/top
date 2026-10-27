# internal/cli

The complete `topctl` command tree (docs/contracts/cli.md, ADR-0011). In
Milestone 1 every command from the contract table exists as a **validated
stub**: it parses and validates its flags and arguments, loads TOP's
runtime config via `internal/config`, honors the global flags, and prints
the versioned output envelope. No stub touches the store or the exec
layer — that wiring is M2/M3 work (see "How stubs get replaced" below).

## Layout

| File | Contents |
| --- | --- |
| `root.go` | cobra root, global flags, config load, `invocation()` |
| `execute.go` | `Execute(argc) → exit code`, per-run context, envelope emit helpers |
| `exit.go` | exit-code constants, `ExitError`, `Options` (stream injection), TTY probe |
| `render.go` | `Envelope`/`ErrEnvelope`, `RenderEnvelope`, `RenderErrEnvelope`, `ParseFormat` — the only place output is shaped |
| `confirm.go` | `--confirm` gate + interactive prompt (ADR-0009) |
| `stubs.go` | every command, grouped exactly like the cli.md table |

## Structure

`Options{Version, Stdin, Out, Err, StdinIsTTY}` injects everything
`Execute` needs; the real binary (`cmd/topctl`) supplies only the
ldflags-injected build stamp. `ExitError{Code}` carries the contract exit
code through `RunE` into `Execute`'s single mapping point:

- read-only stubs (status, diff, history, logs, doctor, backup
  list/verify, recover diagnose) → **0** with the stub envelope,
- mutating stubs (install, uninstall, reconcile, deploy, backup create,
  recover restart/redeploy/restore, controller run) → **3**
  ("operation failed" in the contract code space: the invocation was
  well formed, the operation cannot succeed today; 2 is reserved for the
  caller's mistake),
- invalid usage/validation → **2** with an error envelope and a
  `--help` hint,
- ExitDrift (1) and ExitUnreachable (4) have no stub producer; the
  constants are the contract's single source of truth.

JSON output is always one compact line:
`{"schemaVersion":1,"command":"status","message":"not implemented: planned M2/M3"}`.
`render.go` owns both that shape and the human text renderer; golden
tests in `render_test.go` and `cli_test.go` pin both.

## Why mutating stubs exit 3, not 2

The stub *worked*: flags validated, config loaded, gate satisfied — the
operation itself is what cannot succeed. Reporting 2 (usage) would teach
agents to "fix" a correct command line; reporting 0 would lie about a
mutation that never happened. 3 is the honest rung, and the JSON error
code `not-implemented` distinguishes it from a real M2-era failure.

## Confirmation scaffolding

Gated commands (install, uninstall, backup create, recover
restart/redeploy/restore, plus reconcile when `--prune` or
`--include-controller` is set) call `cctx.requireConfirm`:

- `--confirm` → proceed (deterministic path for agents/CI, ADR-0009);
- stdin is a TTY → interactive `y/N` prompt;
- otherwise → exit 2 with re-run guidance.

The gate runs at flag-validation time only; persisted audit artifacts
and tiered per-flag behavior (`--purge-data` etc.) arrive with the real
operations in M2. Tests drive both interactive branches with captured
stdio and a forced TTY bit (`Options.StdinIsTTY`) — no PTY harness
needed — see `TestConfirmPromptPaths`.

## Global flags

- `-o json|text` (default `text`) — validated in `persistentPreRun`.
- `--config path` — strict load through `internal/config.Load`.
- `--redact=on` — reserved: only `on` is accepted (ADR-0008).
- `--schema-version` — reserved: only the current `JSONSchemaVersion`
  (1) is accepted until negotiation has a second producer.

## How stubs get replaced by M2/M3 service wiring

Each command in `stubs.go` is a thin closure around `cctx` with exactly
three responsibilities left to add: construct the M2/M3 application
service (store/backend/reconciler/recover), call it, and map typed
domain errors onto `ExitError` codes. The pieces that should *not*
change are the flag surface, validation, envelope rendering, exit-code
mapping, and the confirm knot — those are already contract-complete and
golden-tested, so a milestone lands as one body swap per command plus
new data fields in `Envelope` (the `Data` free-form payload grows per
command; bump `JSONSchemaVersion` only if an existing field's shape
changes). Binary naming: `topctl`, never `top` (ADR-0011) — the rename-
safe assertion is `TestHelpMentionsTopctl`.
