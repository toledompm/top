# Backups

> Status: **draft** — target behavior (Milestone 5). Scope decisions:
> ADR-0007 (mechanism) and ADR-0013 (boundary).

## The one-line boundary

**TOP backs up TOP.** The automated backups cover TOP's metadata — platform
state, operation/deployment history, resource ledger — and *not* your
applications' data (volumes, databases). Application data needs its own
strategy (see the cookbook below).

## What is backed up

| Included | Excluded |
| --- | --- |
| SQLite DB (`top.db`) via consistent online backup | Secret values (`/var/lib/top/secrets/`) — names + checksums manifest only |
| Backup registry + watermarks | Git credentials (re-provisioned per runbook) |
| (Redacted) config | Application volumes/databases |

## Mechanism

- Scheduled daily by the controller: `VACUUM INTO` a timestamped file in
  `/var/lib/top/backups/` (consistent, WAL-safe — never a raw file copy).
- After creation: integrity verification (`PRAGMA integrity_check` on a
  restore-opened copy) and optional off-host copy (rsync/SSH target in
  config).
- Retention: 14 daily + 8 weekly (configurable).
- Manual: `topctl backup create`, `topctl backup list`, `topctl backup verify`.
- Metrics: last-success timestamp, last-verify result — a stale backup is a
  first-class alertable condition (ADR-0010).

## Restoring

See [recovery.md](recovery.md) — `topctl recover restore` with overwrite
safeguards.

## Application-data cookbook (not automated in v0)

- Per-app database dumps as ordinary TOP-managed cron workloads writing to a
  mounted volume you snapshot at the host level.
- Host-level filesystem/LVM/ZFS snapshots of `/var/lib/dokku/data/storage/`.
- Off-host sync of storage dirs while the owning app is stopped (quiesce
  first; Dokku storage survives app rebuilds but live DB files don't like
  naive copies either).
