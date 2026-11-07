# internal/cli

The complete `topctl` command tree (docs/contracts/cli.md, ADR-0011).
Introduced in Milestone 1 as **validated stubs**, most bodies are real
after M2/M3 (doctor, install, uninstall, logs, diff, reconcile,
deploy, history, controller run — see the task notes in `cli_test.go`
for who landed when). The command that remains a stub (`status`) still
parses and validates flags and arguments, loads TOP's runtime config via
`internal/config`, honors the global flags, and prints the versioned
output envelope without touching the store or the exec layer.

## Layout

| File | Contents |
| --- | --- |
| `root.go` | cobra root, global flags, config load, `invocation()` |
| `execute.go` | `Execute(argc) → exit code`, per-run context, envelope emit helpers |
| `exit.go` | exit-code constants, `ExitError`, `Options` (stream injection), TTY probe |
| `render.go` | `Envelope`/`ErrEnvelope`, `RenderEnvelope`, `RenderErrEnvelope`, `ParseFormat` — the only place output is shaped |
| `confirm.go` | `--confirm` gate + interactive prompt (ADR-0009) |
| `stubs.go` | every command, grouped exactly like the cli.md table |
| `reconcmd.go` | the T-309 shared wiring: desired-state resolution (gitsource+spec), store opener, reconciler construction, renderers, exit mapping |

## Structure

`Options{Version, Stdin, Out, Err, StdinIsTTY}` injects everything
`Execute` needs; the real binary (`cmd/topctl`) supplies only the
ldflags-injected build stamp. `ExitError{Code}` carries the contract exit
code through `RunE` into `Execute`'s single mapping point:

- the read-only stub (status) → **0** with the
  envelope (diff and history left the stub set in T-309 — diff is the
  read-only computed plan, history reads the operation ledger, both via
  `reconcmd.go`; `logs` left it in T-308 — real body + readiness compose,
  see logs_test.go for its exit rows),
Mutating commands that fail after a well-formed invocation → **3**
  ("operation failed" in the contract code space: the invocation was
  well-formed, the operation cannot succeed (2 is reserved for the
  caller's mistake).
- invalid usage/validation → **2** with an error envelope and a
  `--help` hint,
- ExitDrift (1) now HAS producers (`topctl doctor` failures;
  `topctl diff` drift is also 1 since T-309). ExitUnreachable (4) has producers too since T-308/309
  (backend/store/git unavailability rows). The constants are the
  contract's single source of truth.

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

Gated commands (install, uninstall; reconcile requires it when `--prune`
or `--include-controller` is set; deploy is gated for the controller's
own app) call `cctx.requireConfirm`:

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

## How service wiring replaces stubs

Each command in `stubs.go` is a thin closure around `cctx` with exactly
three responsibilities left to add: construct the M2/M3 application
service (store/backend/reconciler), call it, and map typed
domain errors onto `ExitError` codes. The pieces that should *not*
change are the flag surface, validation, envelope rendering, exit-code
mapping, and the confirm knot — those are already contract-complete and
golden-tested, so a milestone lands as one body swap per command plus
new data fields in `Envelope` (the `Data` free-form payload grows per
command; bump `JSONSchemaVersion` only if an existing field's shape
changes). Binary naming: `topctl`, never `top` (ADR-0011) — the rename-
safe assertion is `TestHelpMentionsTopctl`.

## The four T-309 bodies (diff / reconcile / deploy / history)

These commands left the stub set in T-309. All four share `reconcmd.go`:
`resolveDesired` materializes the platform repo revision (gitsource mirror
+ strict `spec.LoadDir`, file:line sources best-effort mapped), `orStore`
opens `topctl`'s owns SQLite state only if `<dataDir>/top.db` already
exists (reads never invent a store; absent state = honest empty), and
`orReconciler` wires the production dokku backend + host-file secrets.

- **diff** — read-only `reconcile.DiffOnly` (the same plan phases minus
  every mutating stage; no lease, no record). Exit 1 with any
  create/update/delete/self-drift row or orphan, 0 clean (the cli.md
  script anchor). JSON `Data` is the ServiceDiff list with file:line
  sources; secretRef fields carry path + op marker only.
- **reconcile** — a full `Reconciler.Run` (validate → lease → plan →
  apply → gate → record → prune). `--prune`/`--include-controller`
  require the confirm gate (exit 2 untouched non-TTY); run failures,
  contained per-service failures and lease conflicts (envelope code
  `lease-conflict`, no dedicated exit row yet) are exit 3; git/backing
  unavailability 4. `--dry-run` prints the same shared plan, exit 0.
  Without a state store the store-required refusal is exit 4
  (`reconciler-unavailable`). `--service` narrows the run to that
  service plus its transitive dependencies.
- **deploy** — the contract's "Reconciler (single service)" row: the
  named service + its closed dependsOn set converges toward the resolved
  (or `--revision`-pinned) platform revision through the same run; not
  confirm-gated (it converges toward git, per ADR-0002 — the destructive
  boundary stays the prune consent pair). `--dry-run` identical shape.
  T-406 exception: `core/top-controller` (the controller-self app) IS
  confirm-gated (ADR-0005 human gate) — the run carries
  `RunInput.SelfDeployApproved`, the reconciler's only license to apply a
  revision change to its own deployment; the command echoes the
  old-release preservation fact + the re-converge path (`topctl install`)
  for a rollback. Non-TTY without `--confirm` → exit 2.
  Install also wires `Options.FirstRun` (firstrun.go): phase 5's
  reconciliation handoff — gitsource over the positional repo → strict
  spec load → adopt-aware reconcile run — mirroring internal/install's
  reconHandoff reference.
- **history** — the append-only operation ledger (newest first);
  `historyRows` matches the optional ref against the redacted failure
  evidence recorded per row; failed rows render the T-308 hint
  vocabulary's history half (`topctl logs <ns/name>`, last known-good).
  Absent state = empty list, exit 0.

Class-dependent exit rows (cli.md): 0 ok · 1 drift/diagnose findings ·
2 usage · 3 operation failed (incl. lease-conflict, validation-failed,
failed runs) · 4 git/store/backend unavailable (incl. revision-gone/
unavailable/auth git failures, store schema newer, reconciler wiring).
recon_test drives all of them: real local-git fixture repos +
`exectest.FakeRunner` for dokku (the `splitRunner`), so the suite stays
loopback-only; it skips cleanly on hosts without git.
