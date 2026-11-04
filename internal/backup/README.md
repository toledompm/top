# internal/backup — the backup engine (M5, T-501)

The create/verify/prune half of the BackupProvider contract
(`docs/contracts/backup-recovery.md`; mechanism ADR-0007, scope boundary
ADR-0013: **TOP backs up TOP** — metadata only, application data is the
operator's). No CLI lives here: `topctl backup …` verbs are still the
honest notImpl stubs in `internal/cli/stubs.go` (T-503 wired the
**recover** side, not the backup CLI — the verb wiring below is a
pending owner decision, backlog R8).

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

## Offsite copy — `Offsite.Copy` (M5, T-502)

Composes against `Engine.Create`'s returned `*store.Backup` (path, size,
watermark): ONE rsync invocation syncing ONE FILE to the offsite target —
never dir-sync semantics.

```go
copier := &backup.Offsite{                 // wired from config.Backup.Offsite
    Target:         cfg.Backup.Offsite.Target,        // "user@host:path" (rsync remote syntax)
    ExtraArgs:      cfg.Backup.Offsite.Options,       // whitelist-checked (below)
    Port:           cfg.Backup.Offsite.Port,          // 0 = ssh default; inside the -e command
    TimeoutSeconds: cfg.Backup.Offsite.TimeoutSeconds, // 0 = DefaultOffsiteConnectTimeoutSeconds (15)
    Runner:         execRunner,                       // the exec.Runner seam (rsync resolve on PATH)
    Recorder:       metrics,   // *obs.Metrics or nil (OffsiteRecorder seam)
    Log:            logger,    // *slog.Logger or nil
    Now:            clockOrNil,
}
err := copier.Copy(ctx, backup) // MUST be treated as non-fatal by callers
```

Decisions, as shipped:

- **Mechanism: rsync over SSH, not rclone** (the ADR-0007 open question,
  resolved in the implementation, smallest contract-fitting option; the
  backlog names rsync/SSH as the instrument; no new dependency). Config
  still accepts `kind: rclone` but the wiring (T-503 CLI) must build
  `Offsite` only for `rsync-ssh`; rclone fails at resolution time like the
  v0 vault posture (ADR-0008 pattern).
- **Alarm, not failure.** `Copy` returns the rsync error and the caller
  logs-and-continues: the local backup was created and registered FIRST
  (CopyOffsite contract: "failure alarms but must NOT fail the local
  backup"). Copy never touches the registry, never mutates or deletes the
  local file, never panics on destination-down.
- **Alarm surface = metrics, no registry marker in v0.** The store surface
  is frozen for T-502, so copied-state is process-level:
  `top_backup_offsite_last_success_timestamp` (0 until first success —
  "no fresh timestamp" IS the alertable condition, ADR-0007 stale-backup
  style) and `top_backup_offsite_total{result=ok|error}`. No op-record is
  inserted (same evidence rule as Create: nothing inserted silently).
  **Open seam for the M5 review:** a per-row marker needs a store
  amendment — proposed there as `store.Backup.CopiedOffsiteAt time.Time`
  + `SetBackupCopiedOffsite(path, at)` with a goose migration,
  same-change-family as RemoveBackup (T-501). Until then, "which backup
  files are offsite-copied" is not answerable from the registry.
- **Restricted flag whitelist** (no hairy rsync subtleties): extra
  `Options` are rejected unless each is exactly one value-less flag of
  `-a/--archive`, `-z/--compress`, `-c/--checksum`, `-W/--whole-file`;
  `-e`, transport flags, `--delete`, value-taking flags are refused
  pre-exec. The default argv needs no flags for a single file.
- **Security:** rsync runs only through `exec.Runner` (argv slices; no
  shell, so config values are never interpolated). Authentication is
  SSH's own (`ssh -o BatchMode=yes -o ConnectTimeout=N [-p P]` as the `-e`
  value); no key/password is ever stored, passed, or logged by this
  package — a hung password prompt is impossible; destination-down is a
  clean non-zero exit. Stderr excerpts in errors are bounded and
  FastExec-redacted.

### T-503 composition note (update: restore landed, backup CLI still pending)

What T-503 actually shipped on this package: `Offsite.Pull` (restore-side
lifeline pull of a `user@host:path` spec) and the CLI wiring of
`topctl recover restore`. `topctl backup create` remains notImpl — when
the R8 wiring lands it should flow Create → Copy (non-fatal) → Verify,
mirroring the controller's scheduled tick. Also relevant to
restore-from-offsite: with no per-row copied marker, the recover path can
NOT admission-check "did this backup reach the offsite" from the registry —
it shouldn't need to either (restore admits by watermark + integrity, not
by copy history); the offsite file is an extra lifeline, usable manually
with `rsync -e ssh offsite:path/top.db /tmp` when the local dir is gone.

### Tier-3 ssh-container test shape (for T-504 — specced, not run here)

Script under `test/e2e` (disposable container, never the author's host):
start an sshd container with a keypair mounted; `rsync` a real
`Engine.Create` backup via a real `exec.FastExec` to
`backup@localhost:<port>:...`; assert the remote file exists and its
sha256 equals the local one; then "destination-down" variant (unresolvable
hostport) asserts exit != 0, local file intact, exit-code classification
clean. Env-gated (e.g. `TOP_TEST_OFFSITE_SSH=1`) like the other
real-host-coupled tests.

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

## Wiring the CLI (pending owner decision R8; plan unchanged)

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
