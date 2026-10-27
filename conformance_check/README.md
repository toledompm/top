# conformance_check

TOP's contract conformance harness (T-111): an executable fence between
`docs/contracts/*.md` and the Go reality built to satisfy them. When code
or docs drift, this package fails — before `topctl diff` ever prints a
lie. It is a **meta-package**: plain `package conformance_check`, normal
imports of `internal/*` allowed, and no production code — only assertions
and probes. Other code never imports this package.

## Two layers

**Compile level — `conformance_check.go` (always compiles, zero test
cost).** `var _ X = Y` assertions proving each contract's *declared* Go
shape survives refactoring:

| Assertion | Pins |
| --- | --- |
| `spec.LoadDir` assignment + unnamed-struct result shape | service-spec.md: parser output is exactly `[]domain.Service` + `[]spec.Warning` |
| `store.Store = *sqlite.Store` | state-store.md "Responsibilities" — the interface IS the responsibility list |
| `exec.Runner = *exec.FastExec` / `NewFastExec()` | platform-backend.md "final Go signatures are Runner-based" |
| `secrets.Provider = \*HostFile` / `VaultProvider` | ADR-0008 provider seam |
| `plan.Compute`/`ComputeAll` full signatures | reconciler.md pipeline entry points |
| `uint(1 - cli.JSONSchemaVersion)` const trick | cli.md `"schemaVersion": 1` — a non-1 value stops compiling |

Because this file is behind no build tag, every default `go build ./...`
and `go vet ./...` re-checks it (behavior probes, by contrast, are tagged).

**Behavior probes — `*_test.go` behind `//go:build conformance`.** Run via
`make test-conformance` (`go test -tags conformance ./conformance_check/...
-race -count=1`). The default `go test ./...` stays fast and skips them.

## Probe → contract mapping

| Probe | File | Contract sections pinned |
| --- | --- | --- |
| parse → plan → noop idempotency, over `internal/spec/testdata/valid` | `probe_plan_test.go` | reconciler.md "Idempotent"; plan/README.md invariant; service-spec.md field table produces domain vocabulary |
| self-service drift is report-only (`drifted(self)`, changes kept) | `probe_plan_test.go` | reconciler.md/controller rule, ADR-0005 |
| store lifecycle: operations lifecycle (`domain.CanTransition` + persisted terminal enforcement), observed upsert/get, atomic `RecordReconcileRun` (failed composite ⇒ zero residue on the checkpoint), delivery dedup, lease acquire/renew/expire-takeover/foreign-release `ErrLocked`+holder, schema version reported | `probe_store_test.go` | state-store.md "Responsibilities", "Error semantics", "Illustrative interactions" |
| downgrade protection: seeded newer goose version ⇒ `Open` refuses, `errors.Is(store.ErrSchemaNewer)` + typed `SchemaNewerError{Current, Known}` | `probe_store_test.go` | state-store.md "Migration expectations" |
| exec: `ExitError` typed exit 1, `IsExitCode`, redaction through a real child process (`helper-echo`, built hermetically in `TestMain` — same pattern as `internal/exec`), `ErrNotFound` | `probe_exec_test.go` | platform-backend.md "Error semantics" (`ErrBackend` class lives at the adapter; this pins the exec primitives it composes from); ADR-0009 redaction |
| secrets: hostfile round-trip, 12-hex version identity, `SafeRef` → `secret:<name>`, perms 0600/0700 load-bearing (`ErrPerms` + chmod hint), missing → `ErrMissing`, manifest valueless, Vault seam `ErrUnimplemented` | `probe_secrets_test.go` | secrets/ADR-0008 rules (values never in Git/SQLite/logs/JSON; pure-addition providers) |
| CLI exit codes: version=0, status=0, install unconfirmed=2, uninstall `--confirm`=3, unknown=2; JSON envelopes (ok + error) parse with `schemaVersion: 1` | `probe_cli_test.go` | cli.md "Command → contract mapping" exit-code column, "JSON outputs are versioned objects", mutatadopt-me-not-2 rationale in internal/cli README |
| secrets-never-in-JSON: manifest, secret-ref drift plan, `cli.RenderEnvelope(-o json)` output, exec buffers | `probe_observer_test.go` | contracts README "Redaction", ADR-0008 |
| doc-lint: 8 stable token spot-checks across `service-spec.md`, `state-store.md`, `cli.md` | `probe_doclint_test.go` | the documents themselves (drift *in* the docs) |

## Doc-lint scope & fragility

The token checks are **best-effort spot-checks, not a markdown parser**:
each file must contain a handful of deliberately stable tokens (field
columns like `spec.source.image`, sentinel names like `ErrLocked`). This
is intentionally shallow — full doc-to-struct binding would over-fit to
prose and break on harmless rewording. The budget is the 8 checks listed
in `probe_doclint_test.go`; add more only for tokens you'd expect to be
stable for years. When a failure fires, update the doc **or** the probe
consciously (that decision is information).

## Known gaps

- The plan coverage matrix **skips v0-unobservable fields** (healthcheck,
  env *value* drift, secret rotation via checksums, routing mode, storage
  sizes, env-key removal) — see internal/plan/README.md's matrix and its
  M3 notes; conformance currently asserts only the comparable subset plus
  the noop invariant.
- **M3 instruction: revisit conformance_check.** When
  `internal/backend/dokku`'s Inspect adapter extends `domain.ObservedState`
  (M3, see plan/README.md gaps 4–6), this package MUST be revisited: add
  probes for the newly comparable fields, re-run `make test-conformance`,
  and extend the doc-lint budget accordingly.
- The CLI probe is a spot-check of the exit-code table, not exhaustive
  coverage of every stub row (internal/cli's own golden suite covers the
  rest); codes 1 and 4 have no stub producer yet and are only pinned as
  constants.
- Location deviation: the backlog leaves T-111's path unspecified; the
  package sits top-level next to `internal/` rather than inside it because
  it consumes only exported API — like an external consumer would. That
  pragmatic choice is recorded here instead of the backlog entry.
