# TOP implementation backlog (authoritative)

One list, ordered. Rules: AGENTS.md §4. Fields: **Deps** (task IDs), **AC**
(acceptance), **Tests**, **Refs** (ADRs/contracts), **Risk** (host-mutation
risk), **Approval** (human gate beyond normal review), **Agent** (suggested
profile). All tasks produce portable artifacts only (ADR-0014).

Legend — Agent profiles: `core-go` (Go domain/infra), `cli-go` (CLI/UX),
`docs`, `test` (harness/e2e), `review` (critic pass).

---

## Milestone 1 — Foundation

- **T-101 Go module + toolchain + CI skeleton.** Repo init, `go.mod`
  (go 1.24 directive; deps pinned Go-1.24-compatible — sqlite v1.46.1, goose v3.26.0, client_golang v1.23.2), golangci-lint config, GH Actions: build/vet/lint/fmt/test +
  ADR-0014 hygiene grep. Deps: M0. AC: CI green on empty scaffold; fork-safe
  (no secrets). Tests: CI itself. Refs: ADR-0006/0014. Risk: none. Agent: core-go.
- **T-102 Domain types.** `internal/domain`: Service, revisions, Operation,
  states, Plan, errors taxonomy. Deps: T-101. AC: types cover every field in
  contracts without I/O imports. Tests: compile-time + doc examples. Refs:
  contracts/*. Agent: core-go.
- **T-103 ServiceSpec parser + validator.** `internal/spec`: strict YAML
  (`go.yaml.in/yaml/v3`, KnownFields), full validation incl. cross-field
  rules, defaults, file:line errors. Deps: T-102. AC: contract examples pass;
  10+ invalid fixtures fail correctly. Tests: unit + golden files. Refs:
  ADR-0004, service-spec.md. Agent: core-go.
- **T-104 Exec layer + Runner.** `internal/exec`: argv-only, ctx timeouts,
  WaitDelay, minimal env, redacting writer, typed exit errors. Deps: T-101.
  AC: no path executes via shell strings; redaction proven. Tests: unit
  (fake binaries, timeout kills, redaction). Refs: ADR-0006. Risk: none.
  Agent: core-go.
- **T-105 Fake exec + dokku shim.** `test/fakeexec` + `test/shim/dokku`
  (argv log, canned outputs, exit-code scripting). Deps: T-104. AC: shim
  drives a full fake "create app" flow. Tests: self-test. Refs: ADR-0012.
  Agent: test.
- **T-106 SQLite store + migrations.** `internal/store/sqlite`: modernc
  driver, WAL/busy_timeout/FK pragmas, goose embedded migrations; schema v1
  (operations, checkpoints, deliveries, ledger, backups, leases); lease +
  delivery-dedup primitives. Deps: T-102. AC: migrate from zero, round-trip
  Operation, concurrent-writer busy handling. Tests: temp-file integration.
  Refs: ADR-0007, state-store.md. Agent: core-go.
- **T-107 Config + secrets store.** `internal/config` (TOP's own config,
  versioned) and `internal/secrets`: a `SecretProvider` interface
  (`resolve(ctx, name) → (value, version)`) with the v0 `hostfile`
  implementation (perms checks, `topctl secrets set` via stdin) — the seam
  for future Vault-style providers. Deps: T-101. AC: wrong perms → loud
  error; values never logged; provider is swappable by config alone. Tests:
  unit. Refs: ADR-0008. Agent: core-go.
- **T-108 Logging + metrics plumbing.** `internal/obs`: slog JSON, LevelVar,
  correlation-ID ctx handler, prometheus private registry + handler. Deps:
  T-101. AC: sample span emits correlated JSON; /metrics serves. Tests: unit.
  Refs: ADR-0010. Agent: core-go.
- **T-109 CLI skeleton.** `cmd/topctl`: all v0 commands as validated stubs with
  `-o json` + exit-code contract + `--dry-run` parsing. Deps: T-102, T-108.
  AC: every command in cli.md exists, prints schemaVersion'd JSON, correct
  exit codes. Tests: golden CLI tests via shim. Refs: ADR-0011, cli.md.
  Agent: cli-go.
- **T-110 Plan/diff engine (pure).** `internal/plan`: desired×observed →
  Plan with field-level changes; secret-safe rendering. Deps: T-102, T-103.
  AC: idempotency property test (apply plan ⇒ empty next plan, on fakes).
  Tests: property/table units. Refs: ADR-0002, reconciler.md. Agent: core-go.
- **T-111 Contract conformance harness.** Tests asserting Go interfaces match
  contract docs (compile checks + behavior probes). Deps: T-102..T-110.
  AC: harness fails on doc/interface drift. Refs: all contracts. Agent: test.
- **T-112 M1 review + docs catch-up.** Amend contracts to code reality,
  statuses, AGENTS.md. Deps: all M1. Agent: review + docs. Approval: owner
  milestone sign-off.

## Milestone 2 — Bootstrap & host lifecycle

- **T-201 Dokku adapter: read slice.** Inspect composition (ps/builds/git/
  apps/ports/storage/network/resource reports → ObservedState). Deps: T-104,
  T-105. AC: tier-2 golden + tier-3 real-Dokku parity. Tests: shim + tier 3.
  Refs: ADR-0001, platform-backend.md. Risk: none (read-only). Agent: core-go.
- **T-202 Dokku adapter: app CRUD.** EnsureApp/Stop/Destroy/HealthyBackend/
  EnsureNetwork. Deps: T-201. AC: idempotent create/adopt; foreign app →
  ErrConflict. Tests: tier 3. Risk: low (test env only). Agent: core-go.
- **T-203 Preflight (`topctl doctor`).** All checks from installer contract,
  actionable output, JSON mode. Deps: T-109, T-201. AC: each failure mode
  reproducible in tests (PATH faking, port binding). Tests: unit + shim.
  Refs: installer.md, ADR-0009. Agent: core-go.
- **T-204 Ownership inventory + ledger.** Snapshot host state; ledger writes;
  two-key deletion rule implementation. Deps: T-106, T-201. AC: snapshot
  diffing works; deletion refuses on label/ledger mismatch. Tests: tier 3.
  Refs: ADR-0009. Agent: core-go.
- **T-205 Phased installer.** `topctl install` phases 0–3 (wiring: dirs, secrets
  dir, git key provisioning flow, network create, repo registration) with
  resume + `--dry-run`. Deps: T-203, T-204, T-107. AC: interrupted install
  resumes cleanly (kill between phases). Tests: tier 3 scripted. Risk: med
  (host dirs) — test-env only. Agent: core-go.
- **T-206 Gated Dokku install.** `--install-dokku` flow: change-list print,
  interactive confirm, upstream bootstrap pinned to supported version, verify.
  Deps: T-205. AC: decline path clean; dry-run complete. Tests: scripted
  (mock bootstrap), manual checklist for real. Refs: ADR-0009. Risk: high
  by design → **Approval: yes, per run on real hosts.** Agent: core-go.
- **T-207 Bootstrap deploy.** Direct Dokku deploy of pinned controller image
  + storage mount; ledger `bootstrap-owned`. Deps: T-206, T-202. AC:
  controller healthy on clean VM (tier 4-lite). Tests: tier 3 + VM script.
  Refs: ADR-0005. Risk: med. Agent: core-go.
- **T-208 Uninstall + purge tiers.** Full teardown per contract incl.
  final ledger diff report. Deps: T-204, T-205. AC: foreign-container
  survival test scripted (acceptance #11). Tests: tier 3 + VM. Risk: high →
  **Approval: yes (destructive).** Agent: core-go.
- **T-209 install-top.sh.** Checksum-verified fetch (or `--build`), exec into
  `topctl install`. Deps: T-205. AC: one-liner install on clean VM. Tests: VM
  script. Agent: core-go.
- **T-212 Platform-repo template + `--init` flow.** Embedded template (mirrors
  `examples/platform-repo/`), render + remote-create (provider-permitting) +
  initial push with one-time write credential; register-only path for
  existing repos. Deps: T-205, T-301. AC: greenfield install with `--init`
  ends at a synced repo; existing-repo install makes zero changes to the
  repo; missing `--init` → clean preflight error. Tests: tier 3 (local git
  server container) + unit. Refs: ADR-0005/0008. Agent: core-go.
- **T-210 Runbooks: installation/teardown.** From contract to final docs,
  validated against the scripts. Deps: T-205..T-209. Agent: docs.
- **T-211 M2 review + owner sign-off.** Deps: all M2. Approval: owner.
  Agent: review.

## Milestone 3 — GitOps controller

- **T-301 GitSource.** Bare mirror, ls-remote resolve, fetch sha, archive
  extraction, deploy-key + known_hosts wiring. Deps: T-104, T-107. AC:
  private-repo clone via key in tier-3 (local git server container);
  host-key mismatch fails loudly. Tests: unit + tier 3. Refs: ADR-0008,
  git-source.md. Agent: core-go.
- **T-302 Poll loop + queue.** 60 s poll, dedup/coalesce, enqueue. Deps:
  T-301. AC: trigger storms coalesce (unit proof). Tests: unit. Refs:
  ADR-0003. Agent: core-go.
- **T-303 Webhook endpoint.** HMAC verify, dedup via store, 202+enqueue,
  event allowlist, body cap. Deps: T-106, T-108. AC: forged → 401; dup →
  single run; payload never reaches exec. Tests: unit + httptest. Refs:
  ADR-0003. Agent: core-go.
- **T-304 Reconciler core.** Pipeline per contract: validate → lease →
  order → diff → apply → gate → record; last-known-good; per-service
  containment. Deps: T-110, T-106, T-202. AC: idempotency + containment
  properties on fakes; lease blocks concurrent runs. Tests: unit + shim.
  Refs: ADR-0002, reconciler.md. Agent: core-go.
- **T-305 Adapter Apply: image services.** `git:from-image` flow incl.
  digest/tag `--force` logic, batched config, storage mount, network
  attach, ports/domains, restart policy, resources. Deps: T-202, T-301.
  AC: tier-3 deploy of a real image, idempotent re-apply. Tests: tier 3.
  Refs: ADR-0001/0002. Agent: core-go.
- **T-306 Adapter Apply: build services.** `git:sync --build-if-changes`
  flow for repo+sha sources. Deps: T-305. AC: tier-3 source build;
  no-change poll leaves no build record. Tests: tier 3. Agent: core-go.
- **T-307 Healthchecks.** app.json check synthesis (archive path) for image
  services + checks:set timings; fallback behavior per contract. Deps:
  T-305. AC: failing check preserves old release (tier 3 proof). Tests:
  tier 3. Refs: ADR-0002, platform-backend.md. Risk: contains a flagged
  unknown — fallback allowed. Agent: core-go.
- **T-308 Readiness + failure reporting.** Readiness composition,
  FailedLogs surfacing, `topctl logs`. Deps: T-201, T-109. AC: broken-image
  scenario yields actionable history entry. Tests: tier 3. Agent: core-go.
- **T-309 CLI wiring: diff/reconcile/deploy/history.** Real commands over
  services; confirmations for `--prune`/`--include-controller`. Deps:
  T-304, T-109. AC: acceptance tests 5 & 6 scripted. Tests: shim + tier 3.
  Refs: cli.md. Agent: cli-go.
- **T-310 Controller daemon.** Loops wiring, HTTP server (webhook+healthz+
  metrics), graceful shutdown, metrics per ADR-0010. Deps: T-302..T-304,
  T-108. AC: soak test 1h on tier 3 with injected triggers. Tests: tier 3.
  Agent: core-go.
- **T-311 M3 review + ADR-0002 conformance pass.** Deps: all M3. Approval:
  owner. Agent: review.

## Milestone 4 — Self-managed core & observability

- **T-401 Example platform repo.** `examples/platform-repo/` fictional specs:
  controller, prometheus, grafana, exporters; provisioned dashboards;
  small-host defaults. Deps: T-307. AC: specs validate in CI; README explains
  every field. Tests: CI validation. Refs: ADR-0004/0010. Agent: docs+core-go.
- **T-402 Adopt-bootstrap reconciliation.** First-run adoption of
  `bootstrap-owned` controller; ledger conversion. Deps: T-207, T-304. AC:
  install → first reconcile converts ownership without redeploy. Tests:
  tier 3/4. Refs: ADR-0005. Agent: core-go.
- **T-403 Observability wiring.** top-internal attachments, scrape config
  rendering, grafana provisioning + secretRef admin. Deps: T-401, T-305.
  AC: prometheus scrapes controller + exporters; dashboard shows reconcile
  metrics. Tests: tier 4 script. Agent: core-go.
- **T-404 E2E: change→webhook→converged; missed→poll.** Acceptance 3+4
  scripted on VM. Deps: T-303, T-402. Tests: tier 4. Agent: test.
- **T-405 E2E: TOP-down survival.** Acceptance 8: kill/stop controller,
  assert workloads+monitoring persist, revive via recover. Deps: T-402.
  Tests: tier 4. Agent: test.
- **T-406 Self-deploy gated path.** Drift report + `topctl deploy
  core/top-controller` flow with failure preservation. Deps: T-304, T-402.
  AC: acceptance in M4 file. Tests: tier 4. Refs: ADR-0005. Risk: high by
  design → gated. Agent: core-go.
- **T-407 M4 review + clean-VM repeat run.** Deps: all M4. Approval: owner.
  Agent: review.

## Milestone 5 — Backups, recovery, hardening

- **T-501 Backup engine.** Create/verify/prune + registry + metrics.
  Deps: T-106. AC: scheduled backup verifies; retention prunes per policy.
  Tests: unit + tier 3. Refs: ADR-0007. Agent: core-go.
- **T-502 Offsite copy (rsync/SSH).** Config + copy + failure alarming.
  Deps: T-501. AC: destination-down → local intact + metric. Tests: tier 3
  (ssh container). Agent: core-go.
- **T-503 Recover suite.** diagnose/restart/redeploy/restore with all
  safeguards. Deps: T-501, T-202. AC: acceptance 9 + restore-safeguard
  proofs (watermark refuse, safety snapshot). Tests: tier 4. Refs:
  ADR-0005/0007, backup-recovery.md. Agent: core-go.
- **T-504 Failure-injection pack.** Scripted matrix from testing-strategy
  (kill mid-deploy, wedged DB, disk pressure, reboot). Deps: T-503. AC:
  acceptance 7 + matrix green. Tests: tier 4. Agent: test.
- **T-505 Fresh-eyes contributor run.** External-style run of acceptance 12;
  fix all doc gaps found. Deps: T-503, docs. AC: checklist signed.
  **Approval: owner coordinates.** Agent: review + docs.
- **T-506 Docs completion + ADR acceptance sweep.** All runbooks final;
  ADRs → accepted; README status → v0. Deps: all M5. Agent: docs.
- **T-507 Release engineering.** goreleaser or equivalent: versioned
  binaries, checksums, install-top.sh pinning, CHANGELOG. Deps: T-506.
  AC: install from release artifact on clean VM. Agent: core-go.
- **T-508 v0 review + tag.** Approval: **owner.** Agent: review.

## Post-v0 candidates (not scheduled)

- **T-601 MCP server** (`topctl mcp serve`): tool boundary ADR first (ADR-0011).
- **T-602 External secret provider integration** (Vault first): provider
  implementation behind the `SecretProvider` interface from T-107 +
  rotation-via-version flow (ADR-0008, owner-directed direction).
- **T-603 Application-data backup hooks** (ADR-0013 extension point).
- **T-604 Gitea/GitLab webhook verifiers** (ADR-0003 interface).
- **T-605 Alertmanager + Loki specs** (ADR-0010 extension points).

---

## Owner decisions (pre-M1) — ALL RESOLVED 2026-10-27, implementation approved

| # | Question | Resolution |
| --- | --- | --- |
| — | CLI binary name | **`topctl`** (top conflicts with top(1)) |
| 1 | Module path | **`github.com/toledompm/top`** confirmed |
| 2 | Label key prefix | **`top.dev/*`** confirmed |
| 3 | Secrets direction | Host-file store v0 via `SecretProvider` interface; **Vault-style external providers are the future** (T-602); SOPS rejected |
| 4 | Controller self-update | Human-gated (`topctl deploy core/top-controller`) confirmed |
| 5 | e2e VM template | **Lima** confirmed |
| 6 | Webhook reachability | Poll-only fully supported as documented default; webhooks are the optimization |
| 7 | Monitoring | **Prometheus-only v0** confirmed |
| 8 | `--init` remote creation | **GitHub only** for v0; one-time PAT supplies the template push. Author's dev/test token lives in gitignored `.local/.env` — never referenced from tracked files |
