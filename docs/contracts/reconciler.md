# Contract: Reconciler

Turns (desired, observed) into operations. Model decisions in ADR-0002;
trigger/queue decisions in ADR-0003.

## Pipeline (one run)

```
fetch → resolve sha → load+validate specs (strict)
  → acquire reconcile lease (in-process mutex + SQLite lease)
  → order services: namespace tier (core → observability → rest), then dependsOn
  → for each service:
        Inspect (backend) → Diff(desired, observed) → if no-op: record
        → Apply → Readiness gate → record per-service outcome
  → advance checkpoint (lastApplied=sha; lastKnownGood=sha if all green)
  → release lease; write run summary (operation record + metrics)
```

## Rules

- **Idempotent**: running the same revision twice yields empty plans. This is
  a tested invariant of every Milestone-3 change.
- **Partial failure is contained**: a failing service records `failed`,
  keeps its last-known-good running (Dokku preserves old containers on failed
  checks), and does not block services that don't depend on it. Dependents of
  a failed service are skipped with reason `dependency-failed`.
- **Readiness gates ordering**: a service's dependents are applied only
  after its own Readiness passes (bounded wait, e.g. 3 min, then fail the
  dependent with reason).
- **Validation failure = whole run fails before any mutation.** One bad spec
  must not take down the fleet; the run stops at the validate step, records
  `failed` with per-file errors, and previous state keeps running.
- **Controller self-service is special-cased** per ADR-0005: drift on
  `core-top-controller` is reported, never auto-applied.
- **Pruning is never implicit**: services absent from Git are reported as
  `orphaned`; removal only via `topctl reconcile --prune` (non-`core`) with
  confirmation.

## Diff output (shared with `topctl diff`)

Per service: `create | update | delete(prune only) | noop | drifted(self)`,
then field-level changes: `revision: shaA → shaB`, `env: +FOO, ~BAR (secret
ref), -BAZ`, `storage: +data:/prometheus`, etc. Secret values never appear —
only reference names and change markers.

## Retry & backoff

Transient backend errors retry per ADR-0003 (5 tries, capped exp backoff).
Non-retryable: validation, ownership conflict, missing secret reference —
fail fast, no retry, clear error text with the offending file/field.

## Metrics emitted (ADR-0010)

`top_reconcile_duration_seconds`, `top_reconcile_total{result}`,
`top_service_drift{service}` gauge, `top_deploy_total{service,result}`,
`top_deploy_duration_seconds{service}`, `top_last_successful_reconcile_timestamp`.

## Example run record (JSON, from `topctl history`)

```json
{
  "operationId": "op_01J…",
  "kind": "reconcile",
  "actor": "webhook",
  "correlationId": "gh-delivery-9f2…",
  "revision": "a1b2c3…",
  "state": "failed",
  "services": [
    {"service": "core/top-controller", "result": "noop"},
    {"service": "observability/prometheus", "result": "succeeded",
     "changes": ["revision shaA→shaB"], "durationMs": 42100},
    {"service": "observability/grafana", "result": "failed",
     "error": "readiness: checks failed (3 attempts): http 502", "hint": "top logs observability/grafana; last known-good still serving"}
  ]
}
```
