# Reviving TOP

The guiding rule: **revival never depends on TOP being healthy.**
Everything here runs from the host terminal using only Dokku, the
filesystem, and the SQLite file.

## First: is anything actually wrong with your apps?

If TOP is down, your workloads are probably fine — Dokku apps are plain
Docker containers and Dokku runs no daemon (ADR-0001). Verify:

```bash
dokku ps:report <app>          # per app
docker ps                      # ground truth
```

## Reviving the controller

```bash
topctl doctor                   # read-only checks: dokku health, controller
                                # app state, sqlite integrity, config
dokku ps:restart core-top-controller   # manual revive
```

The controller app also carries `--restart=always` from the bootstrap
deploy: after a crash or host reboot Docker brings it back without any
command. When `dokku ps:restart` is not enough, re-run
`topctl install <repo>` — the installer's phases are idempotent and
adopt what exists, completing a partially-run bootstrap.

## Rebuilding TOP's metadata

TOP's SQLite DB needs no restore ritual: its recorded state is
reconstructible by re-running operations (`topctl install`, then a
`topctl reconcile --now`), which is idempotent — the platform converges
on current Git state again. An operator snapshot of the file (the backup
recipe in [`backups.md`](backups.md)) keeps the operation ledger and
resource history; restoring one is the same file-level dance:

1. `dokku ps:stop core-top-controller`.
2. Copy the snapshot over `/var/lib/top/top.db` (same file, same
   permissions — 0600).
3. `dokku ps:restart core-top-controller`, then `topctl reconcile --now`.

If a repair goes wrong, the old file can be pulled back from the snapshot
— a failed restore leaves the original file alone.

## Scenario index

| Symptom | Likely cause | Path |
| --- | --- | --- |
| Controller app exited | bad config, wedged DB, OOM | `topctl doctor` → fix named cause → `docker ps` / `dokku ps:restart` (and check the container's restart policy is `always`) |
| Bad controller deploy | failed checks kept old release; controller spec drifted | `topctl deploy core/top-controller` (human-gated, ADR-0005) |
| TOP DB lost/disk replaced | — | re-provision host → `install` (rebuilds metadata; reconcile is idempotent); an operator snapshot of `top.db` restores the operation ledger |
| Whole host lost | — | new host → `install`; **app data and secrets are re-provisioned separately, by the operator** (ADR-0013/0008) |
| Reconciles keep failing | bad spec in Git | fix in Git; TOP converges on next run (previous state kept running) |
| Dokku itself broken | host issue | follow Dokku's own docs; TOP resumes once `dokku version` works |

## What diagnosis can see without TOP

`topctl doctor` output is terminal text built from `dokku` commands,
file checks, and SQLite inspection — no Prometheus, no Grafana, no
controller. The failure-injection scripts (`test/e2e/t504-*.sh`) run
their health checks through it.
