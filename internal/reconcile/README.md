// SPDX-License-Identifier: MIT

// Package reconciler core — internal/reconcile (T-304).

# internal/reconcile (T-304 — reconciler core)

One `Reconciler.Run(ctx, RunInput) (RunResult, error)` pass implements the
pipeline of `docs/contracts/reconciler.md`: validate → lease → order → diff
→ apply → gate → record → prune → report. It owns NONE of the parsing: the
caller (gitsource/webhook/CLI) hands the already-resolved `domain.Service`
bundle and revision in `RunInput` — no git, no disk, no HTTP here.

## Phase mapping vs the contract

| Contract step | Package | Notes |
| --- | --- | --- |
| fetch → resolve sha → load+validate specs | caller (gitsource + spec) | `RunInput.Specs/Revision`; this package re-validates strictly (`validateRun`, reconcile.go/order.go) |
| acquire reconcile lease | `store.AcquireLease` (LeaseName `"reconcile"`, TTL 10 m) | held lease → `LeaseConflictError` (wraps `domain.ErrLocked`); nothing mutates, nothing recorded |
| order services | `order()` (order.go) | namespace tier (core → observability → rest, ADR-0004), then Kahn topo over intra-namespace `dependsOn`; cycles → `domain.PlanConflictError`; deterministic tie-break |
| Inspect/Diff | `plan.ComputeAll` (internal/plan, REUSED — never reimplemented) | observed = backend Inspect + ledger-derived orphan candidates; result is the `RunResult.Plan` |
| apply / record per service | `applyAndGate` / `applyService` (apply.go) | per-service containment: failure → outcome `failed`, dependents skip `dependency-failed`, independent services continue |
| readiness gate | `awaitReadiness` (apply.go) | `Inspect` until deployed+running(+build not failed + revision/image matched when comparable) under `ReadinessDeadline` (default 3 m), paced by the injected `Clock` (default 5 s) |
| advance checkpoint / record | `recordRun` (record.go) | atomic `RecordReconcileRun`; per-service last-known-good advances only for green outcomes (image services store the image reference, git services the run sha); the map merges, never shrinks |
| prune | `pruneStep` + `PruneGate` (prune.go) | decisions only; deletions execute behind `inventory.Deletable`'s two-key rule (default gate: `LedgerPruneGate`), mirroring internal/install |
| run summary | record.go | operation record pending→running→terminal + `ReconciliationRun`; `topctl history` JSON matches the contract example |

## Harness-independent seams (defined LOCALLY, structural typing)

Defined in `seam.go`; every real implementation must only *have* the
methods — no import of this package required beyond types we share
(`internal/domain`):

- `Backend` — the reconciler's runtime seam. Mirrors today's adapter
  (EnsureApp/Inspect/LastGoodRevision/Stop/Destroy/DeployImage/
  EnsureNetwork/EnsureStorageEntry/AttachStorage/DetachStorage) plus NEW
  convergence methods required of internal/dokku THIS WAVE (exact shapes
  in seam.go; `config:set --no-restart` semantics etc.):
  `SetConfigEnv, SetSecretEnv, SetPorts, SetDomains, SetRestartPolicy,
  AttachNetwork`.
- `StateStore = store.Store` — the persistence seam (aliased: v0 has one
  SQLite implementation; the name is the seam, the type is the contract).
- `Clock` (`Now`, `Wait`) — pacing/deadlines; `WallClock` default, fake in
  tests so --race never sleeps.
- `SecretResolver` — name → value at apply time (ADR-0008); values pass
  straight to `SetSecretEnv`, never logged.
- `PruneGate` — the ADR-0009 two-key delete rule; default
  `LedgerPruneGate{store, snapshot func, namespaces}` delegating to
  `inventory.Deletable`. Prune NEVER deletes directly.
- `SnapshotFunc` — the fresh host inventory for the default gate.

## Lease case semantics

- Acquired `reconcile` lease (renew=false, TTL 10 min) before any
  observation or mutation; released with `context.WithoutCancel` so a
  cancelled run still releases. A release failure can't mask the result —
  the TTL expires regardless.
- Another holder → `LeaseConflictError{Lease, Holder}`; `errors.Is(err,
  domain.ErrLocked)`. No operation record is written for a run that never
  started (fixing up the history with a zero-work failed run would be
  noise; revisit with T-310).
- In-run cancellation: the apply loop stops; the run reports
  `RunFailed` (missing outcomes are "not attempted", not skips).

## Prune consent-pair semantics (ADR-0009)

Prune is never implicit. Deletion requires ALL of:

1. `RunInput.Prune` (the CLI `--prune` flag),
2. `RunInput.Confirm` (the `--confirm` destructive confirmation),
3. the two-key gate passing per candidate (owned ledger row + fresh
   snapshot agreement).

`--prune` without `--confirm` is a fail-fast `ErrInvalid` run error before
the lease; gate refusals surface per candidate in `RunResult.PruneRefusals`
(with the inventory reason string). Core (+ the self ref) is never pruned
(`plan.ComputeAll` enforces; this package passes through). Deleted apps
drop their ledger row after a successful destroy.

## Controller-self special case (ADR-0005/0006)

The `core/top-controller` ref is report-only in this wave: plan drift on
it → outcome `skipped`/`self-drift-reported` with the change list as the
report; converged → `noop`. No apply path exists for it until the M4
adopt path (T-172).

## Applying (what a service's apply does)

`EnsureApp → ledger(ADR-0009 app row) → field changes from the plan →
DeployImage only when the revision changed (DeployImage is not
no-op-safe) → readiness gate`. Field families: plain env (additive set),
secret refs (resolve → set), ports/domains (full replace), networks
(attach only; removals left as sink), storage (ensure entry under
`<VolumeRoot>/<app>-<entry>` — a missing `VolumeRoot` REFUSES to mount
un-owned host dirs), restart policy. Source-build revision changes are a
CONTAINED failure this wave (the `git:sync` seam is T-303's).

## Idempotency

Same inputs after a green run diff to zero changes (tested with the fake
backend: its Inspect reflects every apply). Plan parity with `topctl diff`
is inherited from internal/plan — same `domain.Plan`.

## Testing

Fakes only (`fake_test.go`): fake Backend mutating an in-memory host map,
fake Store implementing exactly the seam methods used (unexpected-store
method = panic via the embedded nil interface), ControlledClock (Wait
advances logical time instantly), fake PruneGate + a real-delegation test
of LedgerPruneGate over a canned snapshot. `go test -race ./internal/reconcile`.
No dokku, no git, no HTTP — per docs/architecture/testing-strategy.md.

## Open items

- T-310: the run-entry canon — holder id default, exit codes
  (LeaseConflict → code, validation → code), and whether a
  lease-blocked run should append a history row.
- Env value drift detection and secret rotation still wait on the
  backend env/secret checksum work (documented in internal/plan's README).
- Network detach, custom-healthcheck, port host-pin semantics: report
  only until the backend exposes the equivalent convergence verbs.
