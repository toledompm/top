# Contract: BackupProvider / Recovery

Two related contracts: making consistent metadata backups, and recovering TOP
without TOP. Scope boundary: ADR-0013 (metadata only). Mechanism decisions:
ADR-0007.

## BackupProvider

**Responsibilities**: create consistent SQLite backups of a live DB; copy
off-host when configured; retain per policy; verify.

**Operations**

- `Create(ctx) (Backup, error)` — `VACUUM INTO` a timestamped file under the
  backup dir; records watermark = newest OperationID + revision; fsyncs;
  registers in the backup registry.
- `CopyOffsite(ctx, Backup) error` — optional configured target
  (rsync+SSH / rclone remote). Failure alarms but does not fail the local
  backup.
- `Verify(ctx, Backup) error` — open the backup read-only, run
  `PRAGMA integrity_check`, confirm watermark present. Scheduled for the
  newest backup; result recorded + exported as a metric.
- `Prune(ctx, policy) (deleted []Backup, error)` — retention (14 daily /
  8 weekly default).

## Scheduling

**v0 scope: operator-managed.** TOP runs no backup scheduler, offers no
backup CLI verbs, and wires no offsite copy in v0. The canonical v0 recipe
is the operator-managed cookbook in `docs/operations/backups.md` (a SQLite
dump/`VACUUM INTO` + retention prune). The engine (`internal/backup`)
ships as a tested internal library; composing it into a controller
daily sweep and/or `topctl backup` verbs is a post-v0 owner decision that
amends this section.

**Engine composition (T-501, shipped):** the engine entered the codebase
as library-only, caller-invoked code: nothing inside TOP calls it in v0.
The post-v0 wiring wave composes `Engine.Create/Verify/Prune` (and the
offsite copier) on exactly the seams this section describes — same code
path for CLI and controller. The contract's "watermark = newest
OperationID + revision": the registry row carries the OperationID; the
revision is already stored on the operation record the watermark points
at, so the registry schema stays as-is.

## Recovery (independent — no controller, no monitoring)

Runs from `topctl recover` on the host; depends only on Dokku CLI, filesystem,
and the SQLite file.

- `diagnose` — read-only. Checks: dokku presence/version; `core-top-controller`
  app existence/state (`ps:report`, `logs --num 200`, `logs:failed`); SQLite
  file openable + integrity; config parses; secret store permissions; listener
  conflicts; disk headroom. Output: findings list, each with severity +
  suggested action.
- `restart` — `dokku ps:restart core-top-controller`, then wait for health.
- `redeploy [--revision sha]` — redeploy the controller via Dokku directly
  (default revision: last-known-good from the DB, else bootstrap-pinned).
- `restore --backup <file>` — safeguards in order: (1) refuse unless
  controller is stopped (stops it via Dokku); (2) refuse if the live DB's
  watermark is newer than the backup's without `--force`; (3) snapshot the
  current DB aside; (4) replace, run migrations, restart controller, verify
  health; (5) print a reconciliation hint (`topctl reconcile --now`).

**Failure behavior**: every step is idempotent and reports the exact command
equivalent for manual execution — recovery output doubles as a runbook.

## Example (fictional)

```console
$ topctl recover diagnose
✓ dokku 0.38.31 healthy
✗ core-top-controller: container exited (1), last deploy 3h ago
  → last log lines suggest: cannot open /var/lib/top/top.db: disk I/O error
  → suggested: check disk space (topctl recover shows 97% used); then `topctl recover restart`
✓ sqlite: /var/lib/top/top.db integrity ok (but stale: newest op 41h old)
```

## Engine clarifications (M5, T-501 — status: PROPOSAL, not yet a blessing)

These are engine-facing amendments the `internal/backup` implementation
ships with (additive; ADR-0007 is untouched). Status stays proposal until
owner sign-off during the M5 review task.

1. **Backup engine location**: `internal/backup` ships
   Create/Verify/Prune over the StateStore's backup-registry slice
   (`store.Store`). Post-v0, the CLIs and the controller are composed
   onto it; nothing else reimplements the mechanism.
   The engine is library-only in v0 — no TOP component calls it.
2. **Create watermark**: registry row `watermark_op` = newest operation ID
   at snapshot time (empty store → ""; a zero-op backup of a zero-op store
   is a valid backup). The revision is carried by the operation record, so
   the registry schema is unchanged.
3. **Same-second creates**: timestamped filenames get numeric suffixes
   (`top-<ts>-1.db`); the backup directory is append-only, a re-run never
   overwrites a real backup.
4. **Verify** opens the file strictly read-only (no migration, no
   "repair"), checks header magic + `PRAGMA integrity_check` + watermark
   presence, and always stamps the registry result. Unregistered paths are
   refused (`ErrNotFound`).
5. **Prune policy shape**: `Policy{KeepDaily, KeepWeekly (+MaxAge
   valve)}` = the ADR-0007 counts (14/8 default), computed as "newest
   daily / weekly per distinct day/ISO week" (exact algorithm in
   `internal/backup/prune.go`, boundary-tested). Prune deletes the file
   first, then the registry row (`RemoveBackup`, state-store contract
   amendment shipped in the same change).
6. **Prune consent**: the engine computes (`Engine.Plan`) and applies
   (`Prune`); it does not prompt. A future CLI = "Plan → render →
   confirm → Prune"; a future scheduled tick Prunes directly — the
   configured policy is the operator's standing consent. Neither exists
   in v0 (operator-managed recipe).
7. **CopyOffsite stays hereby unbuilt in T-501** (the contract keeps it as
   a separate engine-adjacent operation; T-502 composes an offsite-copy
   step against `Engine.Create`'s returned `store.Backup` — path, size,
   watermark — and records failures on the same metrics without failing
   the local backup).

## Offsite copy (M5, T-502 — status: PROPOSAL, not yet a blessing)

What actually shipped (`internal/backup/offsite.go`, README-documented):

1. **Mechanism resolved: rsync over SSH** (the ADR-0007 open question,
   resolved in the implementation as the smallest contract-fitting
   option). ONE rsync invocation syncing ONE FILE (`Offsite.Copy`) —
   never dir-sync semantics. `kind: rclone` stays config-known but
   unwired (fails at resolution like the vault posture —
   **owner decision R6 RESOLVED 2026-11-07: rsync-SSH stands**).
2. **Alarm, not failure**: the caller logs-and-continues — the local
   backup is created and registered first; Copy never touches the
   registry or the local file.
3. **Alarm surface is process-level metrics** (no registry marker in
   v0): `top_backup_offsite_last_success_timestamp` +
   `top_backup_offsite_total{result=ok|error}`. The per-row marker —
   `store.Backup.CopiedOffsiteAt` + `SetBackupCopiedOffsite` + a goose
   migration — is a proposed store amendment (**owner decision R6
   RESOLVED 2026-11-07: the marker stays unwired in v0**, same change
   family as T-501's `RemoveBackup`). Nothing wires offsite copies in
   v0 (operator-managed), so "which backup files are offsite-copied"
   is not answerable from the registry.
4. **Restricted extra-args whitelist**: config `Options` must be
   value-free flags of `-a/--archive`, `-z/--compress`, `-c/--checksum`,
   `-W/--whole-file`; transport flags and `--delete` are refused
   pre-exec. rsync runs only via the `exec.Runner` argv seam (no shell,
   no key/password ever handled).

## Restore mechanics (T-503 — status: PROPOSAL, not yet a blessing)

The restore path that shipped (`internal/recover/restore.go`, wired
behind `topctl recover restore`) makes three v0 proposals beyond the
verbatim safeguard order above; bless or amend them in the same owner
pass (**owner decision R7**):

1. **No operation record is inserted after a restore** — the restored
   DB's watermark equals the backup's exactly (inserting one would make
   the restored store self-referentially newer than its own backup).
2. **Admission is by the FILE, never by registry copy-history**: a
   backup is admitted on watermark + integrity of the file itself, so
   an offsite-only file (`user@host:path`, pulled via
   `backup.Offsite.Pull` before admission) restores even with zero
   local registry rows.
3. **Safety snapshot is a real registered backup** under
   `<dataDir>/backups/safety/` — it MUST succeed before anything
   replaces the live file; a failure aborts with the live DB untouched.
