# 0007 — SQLite state, migrations

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

TOP's metadata (desired/observed state snapshots, operations, reconciliation
checkpoints, webhook deliveries) lives in SQLite on a single
host. We need: safe concurrency for one controller process plus occasional
CLI access, schema evolution, crash recovery.

## Decision

**Driver**: `modernc.org/sqlite` (pure Go). Static, cgo-free binaries make
the single-binary appliance and cross-built controller image trivial;

**Connection policy**: DSN pragmas so every pooled connection inherits them:

```
journal_mode=WAL
synchronous=NORMAL   -- safe in WAL; reconcile re-drives the worst case
foreign_keys=ON
busy_timeout=5000
```

One write connection (`SetMaxOpenConns(1)`) + read pool; all multi-step
writes in explicit transactions.

**Migrations**: `pressly/goose/v3` with embedded plain-SQL migration files,
run at controller start. Migration failures abort startup loudly (never
auto-rollback silently).

## Alternatives considered

- `mattn/go-sqlite3` (cgo) — slightly faster, but cross-compile/static-build
  pain for zero benefit at metadata scale.
- `golang-migrate` — much heavier dependency tree, weaker maintenance
  posture; goose is lean and library-first.
