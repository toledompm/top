# internal/backup — the backup engine (M5, T-501)

The create/verify/prune half of the BackupProvider contract
(`docs/contracts/backup-recovery.md`; mechanism ADR-0007, scope boundary
ADR-0013: **TOP backs up TOP** — metadata only, application data is the
operator's). No CLI lives here: `topctl backup …` verbs are T-503 and wire
these exact seams (see "Wiring the CLI", below).

## API

```go
engine := &backup.Engine{
    DBPath:   "/var/lib/top/top.db",      // the live state store file
    Dir:      "/var/lib/top/backups/",    // destination (ADR-0007)
    Registry: store,                      // store.Store (backup-registry slice)
    Recorder: metrics,                    // *obs.Metrics or nil
    Log:      logger,                     // *slog.Logger or nil
    Now:      clockOrNil,                 // tests; nil → time.Now
}
```

Four operations, one contract each:

| Method | Contract verb | Behavior |
| --- | --- | --- |
| `Create(ctx)` | `Create(ctx) (Backup, error)` | Watermark = newest operation ID; `VACUUM INTO <dir>/top-<ts>.db` (same-second collisions take numeric suffixes — the dir is append-only); fsync file + dir; insert into the registry; stamp success metrics. Refuses a live DB that is not a migrated TOP store (goose table missing → `ErrCorrupt`). |
| `Verify(ctx, path)` | `Verify(ctx, Backup) error` → returns the recorded result string | Unregistered path → `ErrNotFound` (the registry is the source of truth; an unverified file commands no restore trust). Header magic + `PRAGMA integrity_check` over an explicitly **read-only** connection + registered watermark present in the copy's operation history. Always stamps `SetBackupVerified` ("" → `failed: <detail>`) and the verify metric. Corruption classifies `domain.ErrCorrupt`. |
| `Plan(ctx, policy)` | — | Pure consent artifact: what Prune *would* delete. Changes nothing. |
| `Prune(ctx, policy)` | `Prune(ctx, policy) (deleted, error)` | Destructive. Deletes the files, then the registry rows (file first, row second; a missing file still deregisters). First hard error aborts; re-running is idempotent. |

`backup.Policy{KeepDaily, KeepWeekly, MaxAge}` + `DefaultPolicy()` =
ADR-0007's verbatim default **14 daily + 8 weekly, MaxAge off**.

Metrics (obs seam, ADR-0010): `top_backup_last_success_timestamp` /
`top_backup_last_verify_result` (existing M2-era gauges — now wired for
real) plus `top_backup_duration_seconds` / `top_backup_total{result}`.

## Retention algorithm (documented contract; boundary-tested exactly)

1. Daily pass, newest→oldest: first-seen (newest) backup per distinct UTC
   day; the newest `KeepDaily` days survive.
2. Weekly pass over the *remainder*: first-seen per distinct ISO week; the
   newest `KeepWeekly` survive (weekly never re-picks a daily-kept day's
   file — grand-father-father-son with distinct files per class).
3. MaxAge valve: demotes kept-but-older-than-cutoffs — the newest backup
   always survives, retention can never end with zero backups.
4. Rest prunes, oldest-first. Survivors ≤ KeepDaily+KeepWeekly, never 0.

## Two decisions worth knowing before touching this

- **Consent belongs to the caller.** Prune is destructive; the engine
  refuses to invent an interactive prompt. T-503's CLI flow is
  `Plan` → render kept/pruned → confirm (`--yes` in scripts) → `Prune` →
  report. The scheduled controller tick (when the daily-jobs seam exists)
  runs Prune with the configured policy directly — the *config* is the
  operator's prior consent, matching ADR-0007's "scheduled job" language.
- **The registry rows live inside the backups too.** Every backup is a
  full copy of the store, registry included (rows up to that moment).
  Pruned copies carrying now-stale rows are harmless; the LIVE registry is
  the source of truth for what exists.

## Seams (hierarchy discipline)

`Engine` needs no concrete store: `backup.Registry` is exactly the
backup-registry + operation-history slice of `store.Store`, and
`backup.Recorder` is exactly the backup metrics of `*obs.Metrics`. Both
are satisfied by the shipped implementations (compile-proofed in tests;
`internal/store/sqlite` is never imported by the engine itself). Fake
registry/clock/orderable stubs make everything tier-1 hermetic.

## Wiring the CLI (T-503 handoff)

```
topctl backup list            → store.ListBackups (the registry read; no engine needed)
topctl backup create          → Engine.Create (actor: manual; the CLI layer may wrap the call in an OpBackup operation for history — see below)
topctl backup verify [--all]  → Engine.Verify(path); --all iterates ListBackups
topctl backup prune [--policy 14d/8w] [--yes]
    → Engine.Plan → render kept/pruned → confirm → Engine.Prune → report files removed
```

Operation records (state-store contract `kind=backup`) are deliberately
NOT inserted by the engine: inserting an op before/after the snapshot
either misses it or makes a self-referential pending op the watermark.
The CLI/controller layer may wrap `Create` in an operation record for
history/UX; the engine's watermark already carries the operation evidence.

## Tests

`go test ./internal/backup -race` — tier-1 hermetic: real temp-file store
+ temp dirs + fake clock. Verify roundtrips (green / corrupt / unregistered
/ watermark-missing / gone-file), same-second collision suffixing,
non-TOP-DB refusal, metrics outcomes on success AND failure, the pure
retention boundary matrix (daily cut, same-day newest-only, weekly
distinct-file picks, MaxAge demotion + never-starve), and the engine-integrated
prune flow (Plan-mutates-nothing, files+rows deleted, idempotence,
registry-ghost fix-up, default-policy grandfather pattern). Tier-3
(real-Dokku / real controller scheduling) is out of scope here by design:
documented for T-504's e2e, which composes engine + CLI + scheduled ticks
on a disposable VM (see testing-strategy tiers).
