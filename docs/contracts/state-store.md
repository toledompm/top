# Contract: StateStore

Persistence for TOP metadata. One implementation in v0: SQLite
(`internal/store/sqlite`, driver/pragma/migration decisions in ADR-0007).

## Responsibilities

- Record **desired state snapshots**: resolved spec set + revision per
  reconciliation (what we were aiming at).
- Record **observed state** per service: last-known Dokku-reported status,
  revision, health, timestamps.
- Track **operations** (append-only): `OperationID`, `kind` (`install`,
  `reconcile`, `deploy`, `uninstall`, …), `state`
  (`pending|running|succeeded|failed|aborted`), `actor`, `correlationID`,
  `startedAt/finishedAt`, redacted `error`, structured `details` (JSON).
- Track **reconciliation checkpoints**: last fully-applied revision, per-
  service applied revision, last-known-good revision (for rollback).
- **Webhook delivery dedup**: `deliveries(deliveryID PK, receivedAt)` —
  `INSERT OR IGNORE`; zero rows = duplicate.
- **Resource ledger** (ADR-0009): every host resource TOP created
  (`kind`, `identifier`, `labels`, `createdBy operation`).
- **Serialization lease**: `leases(name PK, holderID, expiresAt)` for the
  single-writer reconcile lock and for CLI-vs-controller mutual exclusion.

## Error semantics

- Constraint violations → typed `ErrConflict` (callers decide retry).
- `busy_timeout` handles transient writer contention (ADR-0007); persistent
  `SQLITE_BUSY` after timeout → `ErrLocked` with holder identity in the
  message.
- Corruption/open failure → `ErrCorrupt`; callers surface the
  reconstruct-the-store hint ("wipe the state file and re-run
  `topctl install`; reconcile is idempotent") rather than retrying.

## Migration expectations

- goose, forward-only, embedded SQL; migrations run inside a transaction.
- Store reports its schema version; controller refuses to run on a *newer*
  schema than it understands (downgrade protection).

## Illustrative interactions

```
ReconcileRun(ctx):
  tx := store.Begin()
  ops := tx.InsertOperation(kind=reconcile, actor=webhook, correlationID=deliveryID)
  ... per service: tx.UpsertObserved(...); tx.InsertOperation(kind=deploy, ...)
  tx.SetCheckpoint(lastAppliedRevision=sha, lastKnownGood=sha)   // on success
  tx.Commit()                                                    // all-or-nothing

Webhook intake:
  if !store.RecordDelivery(deliveryID) { return 200, "duplicate" }
```
