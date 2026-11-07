# Backups

> Status: **v0 — operator-managed.** In v0, TOP ships no backup scheduler,
> no `topctl backup` CLI verbs, and no offsite wiring. Backups of TOP's
> own metadata are run by the operator using the recipe below. The
> `internal/backup` engine (create/verify/prune + the rsync/SSH offsite
> copy, `internal/backup/README.md`) is implemented and tested as an
> internal library building toward post-v0 scheduling/CLI/offsite —
> nothing in v0 calls it. Scope decisions: ADR-0007 (mechanism, v0
> scope) and ADR-0013 (boundary).

## The one-line boundary

**TOP backs up TOP.** The backups cover TOP's metadata — platform state,
operation/deployment history, resource ledger — and *not* your
applications' data (volumes, databases). Application data needs its own
strategy (see the cookbook below).

## What is backed up

| Included | Excluded |
| --- | --- |
| SQLite DB (`top.db`) via consistent online backup | Secret values (`/var/lib/top/secrets/`) — names + checksums manifest only |
| Backup registry + watermarks | Git credentials (re-provisioned per runbook) |
| (Redacted) config | Application volumes/databases |

## The v0 operator recipe

Everything you need is SQLite tooling on the host. Both steps come
straight from the engine's documented mechanics, so a post-v0 wiring
lands on the same shape.

1. **Snapshot** (consistent, WAL-safe — never a raw `cp` of the live
   file, ADR-0007):

   ```console
   $ sqlite3 /var/lib/top/top.db "VACUUM INTO '/var/lib/top/backups/top-<timestamp>.db'"
   ```

   `VACUUM INTO` produces a fully-compacted, consistent copy taken
   against a read transaction; it is safe while the controller is up
   (take extra care to quiesce nothing — WAL guarantees the snapshot).
   Rotate the timestamp yourself; the backup directory is append-only by
   convention — never overwrite a previous backup file.

2. **Prune** (the ADR-0007 retention policy: keep the newest backup per
   distinct day for **14 days**, then the newest per distinct ISO week
   from the remainder for **8 weeks**; the newest backup always
   survives). Delete oldest files first, oldest-survivor rule: anything
   older than both the 14-day daily window and the 8-week weekly window
   can go. The exact algorithm (which the pruning wave reuses verbatim)
   is documented in `internal/backup/prune.go` and
   `internal/backup/README.md` §"Retention algorithm".

3. **Off-host copy (your transport, v0)**: an rsync/SSH copy of the new
   backup file to a host of your choice is the lifeline
   `topctl recover restore` accepts (`user@host:path`), so drill the
   restore from the remote side at least once.

## Verify a snapshot

A snapshot is trustworthy only after verification. Open the copy
strictly read-only and check integrity plus watermark presence:

```console
$ sqlite3 'file:/var/lib/top/backups/top-<timestamp>.db?mode=ro' \
    'PRAGMA integrity_check;'
```

`PRAGMA integrity_check` must answer `ok`, and the copy must contain the
operation rows it should (its watermark — the newest operation ID at
snapshot time — is the reference point `topctl recover restore`
admission-checks). The engine's `Verify` performs exactly this plus the
registry stamping, for when that wiring lands.

## Restoring

[`recovery.md`](recovery.md) — `topctl recover restore` **is** real
(M5/T-503): it follows the ADR-0007 safeguard order, refuses to overwrite
a newer database without `--force`, snapshots the current DB first, and
also accepts `user@host:path` to pull the offsite lifeline first.

## Engine reference (library only in v0)

The `internal/backup` package implements the BackupProvider contract as
a tested library: `Engine.Create` (`VACUUM INTO`, watermark = newest
operation ID, append-only directory), `Verify` (read-only integrity +
watermark check, registry result stamped always), `Plan`/`Prune` (pure
consent artifact + destructive apply per `backup.Policy{KeepDaily: 14,
KeepWeekly: 8}`), and `Offsite.Copy`/`Offsite.Pull` (one rsync
invocation syncing one file; failure alarms but never fails the local
backup). Metrics (`top_backup_last_success_timestamp`,
`top_backup_last_verify_result`, …) are emitted by the engine when it
runs — which, again, no v0 component does. See
`internal/backup/README.md` for the API and the post-v0 composition
plan.

## Application-data cookbook (not automated in v0)

- Per-app database dumps as ordinary TOP-managed cron workloads writing to a
  mounted volume you snapshot at the host level.
- Host-level filesystem/LVM/ZFS snapshots of `/var/lib/dokku/data/storage/`.
- Off-host sync of storage dirs while the owning app is stopped (quiesce
  first; Dokku storage survives app rebuilds but live DB files don't
  like naive copies either).
