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

**Scheduling**: controller runs `Create` (+ `CopyOffsite`, `Verify`) daily;
`topctl backup create|verify|list` invokes the same code path from the CLI.

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
