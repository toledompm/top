# internal/recover

`topctl recover` — TOP's **independent recovery path** (docs/contracts/
backup-recovery.md, "Recovery"; ADR-0005). It runs on the host and
depends on exactly three things: **the Dokku CLI, the filesystem, and
the SQLite state file** (`<dataDir>/top.db`). It never depends on — and
has no code path to — a healthy controller, Prometheus, or Grafana.
Every action's output carries the exact manual command equivalent:
*"recovery output doubles as a runbook."*

Status (M5): `diagnose` is fully real and read-only; `restart`/`redeploy`
are real (bounded health waits); `restore` is REAL as of T-503 — the
verbatim ADR-0007 safeguard order with typed pre-mutation refusals
(watermark vs `--force`, schema-newer, corrupt, in-flight lease, missing
live DB) and the offsite `user@host:path` lifeline
(`backup.Offsite.Pull` before admission). `internal/recover/README.md`'s
M2-era "scaffold refuses" text below is history; see
`restore.go`'s doc comment for the exact order.

## Layout

```
recover.go    Options/seams, defaults, the Result runbook record
diagnose.go   all read-only checks (one function per check)
actions.go    Restart / Redeploy (ErrRestoreDeferred was removed in T-503)
restore.go    Restore — the ADR-0007 safeguard order (T-503)
render.go     Summary / RenderText / RenderResultText (doctor-style rows)
probe.go      systemDiskProbe (statfs; dedup TODO recorded there)
```

## Import hygiene and shapes

This package reuses `internal/doctor`'s **Finding/Report/DiskProbe types
directly** (imported — no cycle: doctor never imports recover): one
report shape across topctl, one severity vocabulary. It does NOT import
internal/cli (the arrow goes the other way), and it deliberately does
NOT import internal/backend/dokku — diagnose parses the three report
shapes it needs (`version`, `apps:list`, `ps:report --format json`)
locally, because the recovery path's maximal guarantee is that it works
when every other TOP component does not (ADR-0005's "read inspection +
a handful of direct Dokku operations, nothing more").

The finding methods are free functions (`fail(r, …)`) because method
sets cannot attach to a type alias; the readability is the same.

## Checks (`diagnose`) and their semantics

| ID | Severity fan-out | Semantics |
| --- | --- | --- |
| `dokku/present` | **fail when absent** | Unlike `topctl doctor` (absent → info: an install supplies it), recovery FAILS here: ADR-0005 makes the Dokku CLI the recovery path's only external dependency. |
| `dokku/health` | ok / fail | `dokku version` parses; below the 0.38 floor (ADR-0001) → fail. "Healthy-ish" verdict = version AND `apps:list` both answered. |
| `dokku/apps` | warn | apps:list failed while version worked; later checks degrade from evidence to absence-proof. |
| `controller/state` | ok / **fail** | App existence via apps:list (missing → `controller/missing` fail, hint = redeploy); ps:report `deployed`/`running` (read-only flags, legacy aliases tolerated). Deployed-but-not-running → restart hint; not deployed → redeploy hint. Unparseable report → degraded-to-unknown warn (never a guess). |
| `controller/log-tail` | info | The last of `logs --num 200` surfaces as evidence ONLY when something already failed (contract example: "last log lines suggest: …"). |
| `controller/failed-deploys` | ok / warn / info | `logs:failed` content → warn; empty → ok; plugin unavailable/benign error → info. |
| `store/missing` | info (fresh-host consistent) / fail (controller present) | `<dataDir>/top.db` absent + no controller + no config = *"fresh install assumed"*. Present-but-missing DB with a live app = the volume detached: fail. |
| `store/schema-newer` | **fail** | The typed downgrade-protection error: the store reports `store.ErrSchemaNewer` never a silent downgrade. Hint: run the newer build, restore an older backup. |
| `store/open` | fail | Any other open failure (lock/corrupt/permissions), excerpt + runbook pointer. |
| `store/integrity` | ok / warn | `store.Check` (PRAGMA integrity_check) + staleness: the newest op's completion time vs 6h (reconcile is hourly) — stale → warn, evidence kept ("(but stale: newest op 41h old)" in the contract's words). |
| `config/missing` / `config/valid` / `config/invalid` | info / ok / fail | The canonical `<dataDir>/top-config.yaml` (or the operator's `--config` path): missing → *"fresh install assumed"*; strict-load failure (unknown field/apiVersion) → fail. Highlight: strictness is the point — typos must fail loudly. |
| `perms/top` / `perms/secrets` / `perms/git` (+ `-file`) | ok / warn / info | ADR-0008 permission facts: dataDir 0700; secrets/ and git/ 0700 with 0600 files. READ-ONLY: diagnose suggests `chmod …`, never chmods. `top` = dataDir (fresh-absent → info). |
| `disk/headroom` | ok ≥ 20 GiB / warn ≥ 2 GiB / **fail < 2 GiB** | Same thresholds as doctor (one page of docs covers both); anchored at the dataDir's nearest existing ancestor (never mkdirs). A near-full disk is the common root of dead-controller AND SQLite I/O errors — the fail hint says free space FIRST, restart SECOND. |

Listener conflicts are DELIBERATELY NOT a diagnose check: binding 80/443
is an install-time concern; `topctl doctor` owns the probe (and its
TOCTOU caveat). A diagnose finding would only re-report a host state
recovery cannot act on.

**Mutation note (the single documented exception):** the store open is
migrate-on-open by ADR-0007/store-repo README — recover paths that open
the DB run forward-only migrations; a schema-newer-on-disk store FAILS
the open instead (typed), which is downgrade protection doing its job.

## Actions

- **Restart** — `dokku ps:restart core-top-controller` + a bounded
  health wait (ps:report poll, contract cap 3m; `Options.HealthTimeout`/
  `PollInterval` shrinkable in tests). `--dry-run` prints the exact
  command and executes nothing (CLI invariant).
- **Redeploy** — resolution order: `--revision sha` > repo-wide
  checkpoint `last-known-good` (store) > the controller's per-service
  last-known-good (a sha → `git:sync`+`ps:rebuild`, requires config
  git.url; an image ref → `git:from-image`) > pinned bootstrap image
  (`git:from-image`). A broken store NEVER blocks: it falls back to the
  pinned image and carries the store failure in the result notes
  (ADR-0005: the store is evidence, not a dependency). Bounded health
  wait after every real deploy step.
- **Restore** — REAL (T-503), `restore.go`: the safeguard order is
  verbatim the contract (stop controller → watermark vs `--force` →
  safety snapshot as a real registered backup under
  `<backups>/safety/` → atomic replace + migrate forward → restart +
  bounded health wait + `reconcile --now` hint) with typed,
  pre-mutation refusals: `ErrRestoreWatermark` (older backup over
  newer DB without `--force`), `ErrRestoreSchemaNewer` (forward-only
  migrations cannot read a newer-schema file), `ErrRestoreCorrupt`,
  `ErrRestoreSafetySnapshot` (abort, live DB untouched),
  `ErrRestoreNoLiveDB` (fresh-host flow: install first),
  `ErrRestoreInFlight` (the "restore" lease held).
  `--backup user@host:path` pulls the offsite lifeline via
  `backup.Offsite.Pull` first; admission is by the file's watermark +
  integrity, never by registry copy-history (contract's Restore
  mechanics proposal, owner decision R7).

The ADR-0009 gate lives at the CLI (`--confirm` needed; interactive TTY
prompt otherwise), exactly like every other mutating command.

## Exit codes (docs/contracts/cli.md)

diagnose: any fail finding → **1** (the "drift/diagnose-findings" row);
warns/info/ok never flip it. restart/redeploy: success → 0; failure →
**3** (the exact retry command is in the message). restore: refusal
classes → **3** with the typed code; success → 0.

## Seams (all hermetic; tier-1 testing discipline)

```go
Options{
   Runner exec.Runner          // exectest.FakeRunner in tests — NEVER a real dokku mutation
   DataDir string              // tests: t.TempDir()
   ConfigFile string           // diagnose's validity target
   BootstrapImage string       // "" → DefaultBootstrapImage (placeholder until the installer pins T-2xx)
   DiskProbe doctor.DiskProbe  // reused doctor type; nil → local statfs (probe.go, dedup TODO)
   Now func() time.Time        // staleness math (fixture-certified goldens)
   HealthTimeout/PollInterval  // bounded-wait knobs (tests shrink them)
}
```

`Diagnose` never returns an error — every per-check problem is a Finding
(the doctor Run discipline; all checks run even when some fail). The
bounded-wait guarantee is behavior (`TestRestartHealthTimeout` and
`deadlineExpired`).

## Independence invariant (the point of the package)

`TestDiagnoseBareTempDirIndependence` runs diagnose on a bare tempdir
with a runner whose every dokku call fails "binary not found":
informative findings still land (dokku fail, controller info, fresh-
install store/config infos), NO file appears in the dataDir, and every
recorded subprocess attempt is a dokku READ-ONLY verb. The
`dokku-absent` case is a first-class fixture, not an environment
assumption.

## Known placeholder decisions (M5 must revisit)

- `DefaultBootstrapImage` = `ghcr.io/example/top/topctl:bootstrap-pinned`
  — fictional-but-formatted; T-503 (or the installer's pin) is the
  authority for the real tag (ldflags-injected).
- The DB file name `<dataDir>/top.db` is recover-local; when the store's
  configured path grows a knob, dbPath must follow it (one source).
- `diagnose`'s migrate-on-open: re-verify per the store README (M5 may
  prefer a read-only open for integrity-only diagnosis).
- statfs duplication with internal/doctor: noted as a DEDUPLICATION
  TODO in probe.go (promote into a shared tiny helper when the third
  consumer appears).
