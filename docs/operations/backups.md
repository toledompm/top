# Backups

> Status: **engine real (M5, T-501/T-502)** — the `internal/backup` engine
> (create/verify/prune over the SQLite registry) and the rsync/SSH offsite
> copy are implemented and tested (`internal/backup/README.md`). The
> **`topctl backup` CLI verbs (`create`/`list`/`verify`) are NOT wired
> yet**: they parse and answer the honest not-implemented stub
> (`internal/cli/stubs.go`, the `backup` group) — the engine is reachable
> only from Go code today. That wiring is a pending owner decision
> (backlog R8); this runbook marks each such gap **[CLI pending]**.
> Scope decisions: ADR-0007 (mechanism) and ADR-0013 (boundary).

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

## Mechanism (as implemented)

- `Engine.Create` runs `VACUUM INTO` a timestamped file in
  `<dataDir>/backups/` — a consistent, WAL-safe copy, never a raw file
  copy. The watermark is the newest operation ID at snapshot time.
  Same-second creates take numeric suffixes; the backup directory is
  append-only (a re-run never overwrites a real backup) (`internal/backup/create.go`).
- After creation: verification — the copy is opened **strictly
  read-only**, gets header-magic + `PRAGMA integrity_check` checks and a
  watermark-presence test against the copy's own operation history, and
  the registry result row is always stamped (`internal/backup/verify.go`).
- **Offsite copy (optional, rsync/SSH)**: after a create, the new backup
  file is synced to the configured rsync target
  (`backup.offsite.target: "user@host:path"` in `top-config.yaml`; extra
  args are whitelist-checked — value-free `-a/-z/-c/-W` forms only).
  Offsite failure **alarms but never fails the local backup**: the result
  is recorded in the offsite metrics only
  (`top_backup_offsite_last_success_timestamp`,
  `top_backup_offsite_total{result}`) — `internal/backup/offsite.go`.
  `kind: rclone` is config-known but not wired (**owner decision R6**).
- Retention: `backup.Policy{KeepDaily: 14, KeepWeekly: 8}` — the ADR-0007
  default; daily keeps the newest backup per distinct day, weekly picks
  from the non-daily remainder per distinct ISO week, the newest backup
  always survives, and pruning deletes the file before the registry row
  (`internal/backup/prune.go`). The engine's `Plan` computes what would
  be deleted without touching anything; `Prune` still needs the caller's
  consent — the CLI confirmation on manual runs, or the configured policy
  as standing consent on a scheduled tick.
- Metrics: `top_backup_last_success_timestamp`,
  `top_backup_last_verify_result`, plus
  `top_backup_duration_seconds`/`top_backup_total{result}` (ADR-0010
  set). A stale backup is a first-class alertable condition.

**[CLI pending]** `topctl backup create`, `list`, `verify` (and the
planned `prune`) do not yet call the engine — they exit 2/0 with the
"not implemented" envelope. Manual backups today are either the wired
restore path's own safety snapshots (`<dataDir>/backups/safety/`) or a
Go/CLI wiring away. The intended CLI flow (verbatim from
`internal/backup/README.md` §"Wiring the CLI"):

```console
$ topctl backup create           # Create → offsite Copy (non-fatal) → Verify
$ topctl backup list             # the backup-registry rows (newest first)
$ topctl backup verify [label]   # integrity + watermark (default: latest)
$ topctl backup prune            # Plan → render kept/pruned → confirm → Prune
```

**[CLI pending]** The controller does **not yet** schedule backups on its
own: it has no daily-jobs seam, so scheduling is caller-invoked (the
contract's "M5 proposal" wording in `docs/contracts/backup-recovery.md`;
owner decision R5 covers both the jobs seam and the scheduled-tick
consent).

## Restoring

[`recovery.md`](recovery.md) — `topctl recover restore` **is** real
(M5/T-503): it follows the ADR-0007 safeguard order, refuses to overwrite
a newer database without `--force`, snapshots the current DB first, and
also accepts `user@host:path` to pull the offsite lifeline first.

## Application-data cookbook (not automated in v0)

- Per-app database dumps as ordinary TOP-managed cron workloads writing to a
  mounted volume you snapshot at the host level.
- Host-level filesystem/LVM/ZFS snapshots of `/var/lib/dokku/data/storage/`.
- Off-host sync of storage dirs while the owning app is stopped (quiesce
  first; Dokku storage survives app rebuilds but live DB files don't
  like naive copies either).
