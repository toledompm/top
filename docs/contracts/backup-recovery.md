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

Controller runs `Create` (+ `CopyOffsite`, `Verify`) daily;
`topctl backup create|verify|list|prune` invokes the same code path from
the CLI.

**M5 proposal (engine-facing, T-501 — clarification, not a blessing):**
until the controller grows a daily-jobs seam (`internal/controller` today
has event triggers + a periodic reconcile sweep, no jobs table), scheduling
is **caller-invoked v0**: the controller calls
`internal/backup.Engine.Create/Verify/Prune` exactly as the CLI does, per
this section. A jobs-seam hookup is a later, separate amendment. Also, the
contract's "watermark = newest OperationID + revision": the registry row
carries the OperationID; the revision is already stored on the operation
record the watermark points at, so the registry schema stays as-is.

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

1. **Backup engine location**: `internal/backup` ships Cre­ate/Verify/
   Prune over the StateStore's backup-registry slice
   (`store.Store`). The CLIs and the controller call it; nothing else
   reimplements the mechanism.
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
   (`Prune`); it does not prompt. CLI = "Plan → render → confirm → Prune".
   The scheduled controller tick Prunes directly — the configured policy
   is the operator's standing consent.
7. **CopyOffsite stays hereby unbuilt in T-501** (the contract keeps it as
   a separate engine-adjacent operation; T-502 composes an offsite-copy
   step against `Engine.Create`'s returned `store.Backup` — path, size,
   watermark — and records failures on the same metrics without failing
   the local backup).
