# Recovering TOP

> Status: **real (M5)** — all four verbs are implemented and
> host-independently backable: `diagnose` (read-only), `restart`, and
> `redeploy` since M2; `restore` landed in T-503 behind the verbatim
> ADR-0007 safeguard order (`internal/recover/restore.go`, CLI-wired).
> The guiding rule: **recovery never depends on TOP being healthy.**
> Everything here runs from the host terminal using only Dokku, the
> filesystem, and the SQLite file.

## First: is anything actually wrong with your apps?

If TOP is down, your workloads are probably fine — Dokku apps are plain
Docker containers and Dokku runs no daemon (ADR-0001). Verify:

```bash
dokku ps:report <app>          # per app
docker ps                      # ground truth
```

## The recovery path

```bash
topctl recover diagnose           # read-only: dokku health, controller state,
                               # sqlite integrity, config, permissions, disk
topctl recover restart            # dokku ps:restart core-top-controller
topctl recover redeploy           # redeploy controller at last-known-good
                               # (or --revision <sha> / bootstrap pin)
topctl recover restore --backup /var/lib/top/backups/top-2026-10-27T0300.db
```

`redeploy` resolution order (`internal/recover/actions.go`):
`--revision <sha>` → repo-wide last-known-good from the store → the
controller service's own last-known-good → the pinned bootstrap image. A
broken store never blocks it — it falls back to the pinned image and
carries the store failure in the result notes (the store is evidence,
not a dependency). `redeploy` and `restore` are confirm-gated (ADR-0009:
`--confirm`, or a TTY prompt).

## `restore` — what it guarantees (implemented order)

The safeguard order is verbatim the contract
(`docs/contracts/backup-recovery.md` "Recovery") and ADR-0007:

1. Refuse unless the controller is stopped — the command stops it via
   Dokku itself.
2. Refuse if the live DB's newest operation is **newer** than the
   backup's watermark, without `--force` (typed refusal, before any
   file is touched; restoring equal watermarks is allowed).
3. Snapshot the current DB aside as a real, registered backup under
   `<dataDir>/backups/safety/` (`backup.Engine.Create`) — a failed
   safety snapshot aborts the restore with the live DB untouched.
4. Replace the live DB atomically (same-directory temp file, fsync,
   rename) and run migrations forward.
5. Restart the controller, wait bounded for health, and print the
   reconciliation hint (`topctl reconcile --now`).

Additional refusals, each before any mutation: a corrupt backup file,
a backup written by a **newer schema** than this build (forward-only
migrations cannot read it), a DB file that doesn't exist (fresh-host
flow: `install` first, then restore), and a restore already in
progress. `--dry-run` prints the plan and mutates nothing.

The `--backup` value also accepts **`user@host:path`** — the file is
pulled from the offsite target via rsync first (the "offsite lifeline"
for the case where the local backups dir is gone; `T-503`'s
`Offsite.Pull`). Admission is by the file's own watermark + integrity,
never by registry copy-history — so an offsite-only backup restores even
though the live registry never saw it.

## Scenario index

| Symptom | Likely cause | Path |
| --- | --- | --- |
| Controller app exited | bad config, wedged DB, OOM | `diagnose` → fix named cause → `restart` |
| Bad controller deploy | failed checks kept old release; controller spec drifted | `topctl deploy core/top-controller` (human-gated) or `recover redeploy` |
| TOP DB lost/disk replaced | — | re-provision host → `install` → `recover restore --backup <latest>` → `topctl reconcile --now` |
| Whole host lost | — | new host → install → restore metadata backup (from offsite with `user@host:path` when local dirs are gone) → reconcile; **app data and secrets are re-provisioned separately** (ADR-0013/0008) |
| Reconciles keep failing | bad spec in Git | fix in Git; TOP converges on next run (previous state kept running) |
| Dokku itself broken | host issue | follow Dokku's own docs; TOP recovery resumes once `dokku version` works |

## What recovery can see without TOP

`topctl recover diagnose` output is terminal text built from `dokku` commands,
file checks, and SQLite inspection — no Prometheus, no Grafana, no controller.
The failure-injection scripts (`test/e2e/t504-*.sh`) assert this posture
against the matrix: kill mid-deploy, wedged SQLite, ≥95% disk pressure
(diagnose names `disk/headroom` as a FAIL), and host reboot.
