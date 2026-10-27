# Test strategy

Decision record: ADR-0012. This page is the operational how-to.

## Tiers

| Tier | What | Where it runs | Gate |
| --- | --- | --- | --- |
| 1 Unit | domain, spec parse/validate, diff/plan, reconcile logic, sqlite store, webhook verify/dedup, redaction | every PR, seconds | none |
| 2 Shim | real `exec.Runner` against a fake `dokku` script on PATH (records argv, canned stdout, exit codes) | every PR, seconds | none |
| 3 Real-Dokku | official `dokku/dokku` image, host docker.sock mounted (sibling topology), via testcontainers-go | PRs touching the adapter + nightly; local: `TOP_E2E=1 go test ./internal/backend/dokku/...` | `TOP_E2E=1` |
| 4 Full e2e | disposable VM (Lima template in `test/e2e/`, cloud-init: Docker + Dokku + top) running the scripted acceptance suite | nightly, releases, on demand: `test/e2e/run.sh` | none (self-contained) |

## Failure-injection matrix (tiers 3–4)

| Scenario | Injection | Expected |
| --- | --- | --- |
| Controller killed mid-deploy | `docker kill` during apply | next run resumes/re-diffs; no partial env state (config batching); op recorded `aborted` then superseded |
| Invalid ServiceSpec | bad field / unknown key / both image+build | whole run fails at validate, zero mutations, clear file:line error |
| Failing healthcheck | image with dead /health | op `failed`, old release still serving, `logs:failed` surfaced |
| Webhook storm | 50 duplicate + 5 out-of-order deliveries | dedup → 1 coalesced run; newest sha wins |
| Git outage | block repo network | run aborts pre-mutation; workloads untouched; poll recovers |
| SQLite wedged | `chmod 000` DB dir | controller exits loud; `recover diagnose` names it; fix + restart recovers |
| Disk pressure | fill to 95% in VM | preflight/backup report clear errors; no silent corruption |
| Host reboot (tier 4) | VM reboot | Dokku `ps:restore` revives apps + controller; TOP resumes polling |

## The 12 acceptance tests (mapped)

| # | Test | Tier | Milestone |
| --- | --- | --- | --- |
| 1 | Install from a repo per public docs | 4 | M2/M4 |
| 2 | Reconcile `core` + `observability` | 4 | M4 |
| 3 | Git change → webhook → reconcile | 3 (HMAC/dedup) + 4 | M3/M4 |
| 4 | Missed webhook → poll repairs | 4 (webhook dropped deliberately) | M4 |
| 5 | Manual `diff`/`reconcile` | 3 + 4 | M3 |
| 6 | Invalid spec + failed deploy safety | 1 (validation) + 4 | M3 |
| 7 | Controller restart mid-operation | 4 | M5 |
| 8 | TOP broken → workloads + monitoring survive | 4 | M4 |
| 9 | Independent recovery path | 4 | M5 |
| 10 | Backup + restore metadata | 4 | M5 |
| 11 | Uninstall spares unrelated resources | 4 (pre-seeded foreign container) | M2/M5 |
| 12 | Clean-environment contributor run | 4 on a fresh Lima VM, docs-only | M5 |

## Local (author-only) testing

`.local/env` may set `TOP_E2E_HOST`/`TOP_E2E_SSH_USER` for read-only checks
against a real host. Mutating scenarios require `--allow-host-mutation` +
interactive confirmation and are documented as exceptional. See
`examples/local-setup.example.md`. Per ADR-0014, no tracked test may require
this.

## CI shape (GitHub Actions)

- `pr`: tiers 1–2 (ubuntu), lint/vet/gofmt, docs hygiene grep (ADR-0014).
- `pr-adapter` (path-filtered) + `nightly`: tier 3.
- `nightly` + release tags: tier 4 (VM on the runner; nested-virt note kept in
  the workflow comments; fall back to tier-3-extended if unavailable).
- Forks with no secrets must pass `pr` + `pr-adapter`.
