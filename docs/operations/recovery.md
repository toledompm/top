# Recovering TOP

> Status: **partially real (M2)** — `topctl recover diagnose` is fully real
> and read-only; `restart` and `redeploy` work (minimal, same independence);
> `restore` is scaffold-only: it **explicitly refuses** (exit 3,
> "restore is deferred to M5") until T-503 lands the ADR-0007 safeguard
> order. The guiding rule: **recovery never depends on TOP being healthy.**
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

`restore` (M5): controller must be stopped (it stops it); refuses to
overwrite a newer database without `--force`; snapshots the current DB first;
runs migrations forward; restarts and verifies health.

## Scenario index

| Symptom | Likely cause | Path |
| --- | --- | --- |
| Controller app exited | bad config, wedged DB, OOM | `diagnose` → fix named cause → `restart` |
| Bad controller deploy | failed checks kept old release; controller spec drifted | `topctl deploy core/top-controller --revision <last-good>` or `recover redeploy` |
| TOP DB lost/disk replaced | — | re-provision host → `install` → `recover restore --backup <latest>` → `topctl reconcile --now` |
| Whole host lost | — | new host → install → restore metadata backup → reconcile; **app data and secrets are re-provisioned separately** (ADR-0013/0008) |
| Reconciles keep failing | bad spec in Git | fix in Git; TOP converges on next run (previous state kept running) |
| Dokku itself broken | host issue | follow Dokku's own docs; TOP recovery resumes once `dokku version` works |

## What recovery can see without TOP

`topctl recover diagnose` output is terminal text built from `dokku` commands,
file checks, and SQLite inspection — no Prometheus, no Grafana, no controller.
