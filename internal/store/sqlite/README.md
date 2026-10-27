# internal/store/sqlite

TOP's metadata store (milestone-1 contract: `docs/contracts/state-store.md`;
decisions: ADR-0007). One implementation in v0: SQLite via the pure-Go
`modernc.org/sqlite` driver, embedded goose migrations. The public surface is
`store.Store` (see `internal/store/store.go`) — types come from
`internal/domain`; this package adds no domain concepts.

## Layout

```
sqlite/
├── open.go          Open: DSN pragmas, connection policy, migrations, downgrade check
├── store.go         operations / observed state / checkpoints / deliveries
├── runs.go          RecordReconcileRun (atomic run + outcomes + checkpoint), checkpoint helpers
├── ledger.go        resource ledger (ADR-0009) + backup registry
├── lease.go         serialization lease (single-writer reconcile, CLI mutual exclusion)
├── errors.go        driver-error → contract-class mapping (classify)
├── embed.go         embedded migrations
└── migrations/0001_init.sql   schema v1
```

## Schema (version 1 — goose owns `goose_db_version`)

| Table | Purpose | Notes |
| --- | --- | --- |
| `operations` | append-only operation history | `kind/state/actor/correlation_id/revision`, redacted `error`, JSON `details`; nullable `started_at`/`finished_at` (NULL = zero time) |
| `runs` | reconciliation run summaries | PK = the operation that performed the run |
| `run_outcomes` | per-service results per run | FK `→ runs(operation_id)` ON DELETE CASCADE, PK `(operation_id, ref)` |
| `observed_state` | last backend-reported snapshot per service | PK `ref` ("ns/name"), JSON state, revision + `observed_at` |
| `checkpoints` | reconciliation resume/rollback state | singleton (id=1): repo-wide last-applied / last-known-good + two per-service JSON maps |
| `deliveries` | webhook dedup | `INSERT OR IGNORE`; 0 rows updated = duplicate |
| `resources` | ownership ledger (ADR-0009) | `UNIQUE(kind, identifier)`, state `bootstrap-owned|managed|pre-existing` |
| `backups` | backup registry | path PK, size, `watermark_op`, `verified_at`+`verify_result` |
| `leases` | serialization lease | PK `name`, holder + nanosecond `expires_at` |

Indexes: `operations(state)`, `operations(kind, created_at)`, `runs(correlation_id)`.
Timestamps are stored as UTC Unix nanoseconds (INTEGER); NULL means unset.

## Connection policy (ADR-0007, single-decision documented)

The DSN carries per-connection pragmas so every pooled connection inherits
them: `journal_mode(WAL)`, `synchronous(NORMAL)`, `foreign_keys(1)`,
`busy_timeout(5000)`.

One `*sql.DB` with `SetMaxOpenConns(1)` serializes **all** access — reads and
writes — through one write-capable connection.

- **In-process contention is structurally impossible**: writes queue at the
  pool, so no `SQLITE_BUSY` can occur between TOP's own goroutines, and
  every multi-step write is trivially consistent.
- **Tradeoff**: we give up WAL's free read concurrency. At TOP's metadata
  scale (per-run records, not bulk data) this is negligible; if it ever
  matters, the split layout is `DSN-params`-identical: a second `*sql.DB`
  opened read-only for List/Get workloads. The API never changes.
- **Cross-process contention** (topctl vs controller) still exists: it is
  covered by `busy_timeout=5000`, and persistence past the timeout surfaces
  as `ErrLocked` (below).
- All multi-step writes go through one transaction (`RecordReconcileRun`)
  or are single statements.

## Migration procedure

- Migrations are plain-SQL goose files, embedded
  (`migrations/NNNN_*.sql`); they run inside a transaction per file and are
  invoked from `Open` at controller start and by `topctl recover` paths
  that open the DB (M5 wiring). Failure aborts startup loudly; there is no
  silent auto-rollback (forward-only, ADR-0007).
- `SchemaVersionMax` in `open.go` MUST be bumped whenever a migration file
  is added; it is the downgrade-protection reference.
- The goose version table `goose_db_version` is owned by goose — never
  edited by application code (tests may simulate a newer build there).

## Downgrade rule

On open, the highest applied `version_id` is compared to
`SchemaVersionMax`. A newer on-disk schema returns
`store.ErrSchemaNewer` (`SchemaNewerError{Current, Known}`): run the newer
build, or restore an earlier backup. Running forward-off-a-newer-schema is
a silent downgrade and is deliberately impossible.

## Error semantics (state-store contract)

| Class | Sentinel | Produced by |
| --- | --- | --- |
| Constraint/existence clash | `domain.ErrConflict` | duplicate operation id, ledger `(kind,identifier)`, backup path; `MarkManaged` on a pre-existing entry, identical-entity adoption |
| Invalid lifecycle move | `domain.ErrConflict` | `UpdateOperationState` against the persisted state, per `domain.CanTransition` (terminal states reject further moves; skips too) |
| Persistent writer contention | `domain.ErrLocked` | `SQLITE_BUSY`/`SQLITE_LOCKED` after `busy_timeout`; contested lease acquire/release — **holder identity is in the message** |
| Corruption / open failure | `domain.ErrCorrupt` | ping failure, `SQLITE_CORRUPT`/`SQLITE_NOTADB`, unreadable JSON columns, `Check`'s `PRAGMA integrity_check` result. Never retried — surface `topctl recover diagnose` |
| Missing | `domain.ErrNotFound` | not-yet-observed service, unknown operation, unregistered backup/ledger entry |
| Input validation | `domain.ErrInvalid` | empty ids, nil records, invalid enum values, a failed operation recorded without an error string |
| Schema newer than build | `store.ErrSchemaNewer` | see Downgrade rule |

Everything is wrapped with operation context (`fmt.Errorf("op: %w")`);
`errors.Is`/`errors.As` branch on the sentinels or typed values.

## Testing

`go test ./internal/store/... -race` covers: migration from zero, embedded
version list, operation lifecycle incl. rejected transitions, JSON
round-trips (operations details, observed state, checkpoint maps), delivery
dedup, ledger/backup registry, lease acquire/renew/expire/foreign-release,
downgrade protection, the ADR-0007 pragma set taking effect, and a
two-goroutine contention workload (no `time.Sleep`; the single connection
serializes it). Databases are always per-test temp files.
