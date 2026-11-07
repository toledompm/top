# 0007 — SQLite state, migrations, and metadata backup/restore

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

TOP's metadata (desired/observed state snapshots, operations, reconciliation
checkpoints, webhook deliveries, backup registry) lives in SQLite on a single
host. We need: safe concurrency for one controller process plus occasional
CLI access, schema evolution, crash recovery, and consistent backups of a
*live* database.

## Decision

**Driver**: `modernc.org/sqlite` (pure Go). Static, cgo-free binaries make
the single-binary appliance and cross-built controller image trivial; it
exposes the SQLite Online Backup API and tracks current SQLite.

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
run at controller start and by `topctl recover` commands that open the DB.
Migration failures abort startup loudly (never auto-rollback silently).

**Backup mechanism**: consistent online backup via `VACUUM INTO '<dir>/top-<ts>.db'`
(with the Online Backup API as fallback/progress option) — never `cp` of the
live file. Destination: `/var/lib/top/backups/` (outside TOP's app dirs), with
an optional off-host copy target (rsync/SSH destination in config) executed
after a successful backup.

**v0 scope: operator-managed.** In v0, TOP ships no backup scheduler, no
`topctl backup` CLI verbs, and no offsite wiring: backups are run by the
operator, and the canonical recipe is the SQLite dump/VACUUM + retention-prune
cookbook in `docs/operations/backups.md`. The engine artifact
(`internal/backup` — create/verify/prune + offsite copy, a tested internal
library) is the building block the post-v0 wiring wave composes into a
scheduler and/or CLI; a later amendment to this ADR covers that composition.

**Verification & retention**: verification opens the copy read-only and runs
`PRAGMA integrity_check` plus a watermark check, recording the result and
metrics. Retention: keep daily for 14 days + weekly for 8 weeks (configurable).

**Restore safeguards** (`topctl recover restore --backup <file>`):

1. Requires the controller stopped (the command stops it via Dokku and
   restarts it after).
2. Refuses to restore over a DB whose newest recorded operation is newer
   than the backup's watermark unless `--force` is given.
3. Takes a safety snapshot of the current DB before overwriting.
4. Runs migrations forward after restore if the backup is schema-behind.

## Alternatives considered

- `mattn/go-sqlite3` (cgo) — slightly faster, but cross-compile/static-build
  pain for zero benefit at metadata scale.
- `golang-migrate` — much heavier dependency tree, weaker maintenance
  posture; goose is lean and library-first.
- Litestream continuous replication — good product, premature for v0; the
  backup contract leaves room to add it later.
- Copy-the-file backups — unsafe with WAL; rejected outright.

## Consequences

- Backup covers **TOP metadata only** — the boundary with application data
  is ADR-0013.
- In v0 no TOP component executes backups (operator-managed); the metrics
  and stale-backup alerting described below are engine-level guarantees
  that become live surface when the post-v0 wiring lands.
- Metrics must include last-backup success timestamp and verification result
  (ADR-0010); a stale backup is a first-class alertable condition.
- The restore path is part of the independent recovery contract and is
  exercised in e2e (Milestone 5 acceptance test 10).

## Open questions

- Default off-host copy configuration format (rclone config reference vs
  plain rsync+SSH target) — decide in Milestone 5; contract allows both.
