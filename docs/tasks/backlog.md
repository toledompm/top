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
  (operations, checkpoints, deliveries, ledger, leases); lease +
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
  **Status: DONE 2026-11-03** (`ecd5392`, `7c346bd`) — tier-1 validation
  green (`platformrepo_validate_test.go`); 4 tier-1 packages delivered.
- **T-402 Adopt-bootstrap reconciliation.** First-run adoption of
  `bootstrap-owned` controller; ledger conversion. Deps: T-207, T-304. AC:
  install → first reconcile converts ownership without redeploy. Tests:
  tier 3/4. Refs: ADR-0005. Agent: core-go. **Status: DONE at tiers 1–2
  2026-11-03** (`fba65c4`, `82c7e30`, `145aa40`); clean-VM proof pending
  the soak run (see docs/milestones/m4-review.md §5).
- **T-403 Observability wiring.** top-internal attachments, scrape config
  rendering, grafana provisioning + secretRef admin. Deps: T-401, T-305.
  AC: prometheus scrapes controller + exporters; dashboard shows reconcile
  metrics. Tests: tier 4 script. Agent: core-go. **Status: DONE at tier 1
  2026-11-03** (`3ffd717`, `8d602b6`); live scrape proof pending soak.
  Deviations/proposals recorded in m4-review.md §3 (spec.args, storage
  readOnly, copy-sync, dash-kebab).
- **T-404 E2E: change→webhook→converged; missed→poll.** Acceptance 3+4
  scripted on VM. Deps: T-303, T-402. Tests: tier 4. Agent: test.
  **Status: SCRIPTED 2026-11-03** (`6d487a3`, `test/e2e/t404-webhook-poll.sh`
  + hermetic `lib.sh`/`lib_test.go`); not yet live-run.
- **T-405 E2E: TOP-down survival.** Acceptance 8: kill/stop controller,
  assert workloads+monitoring persist, revival via the restart policy
  (operator posture: `dokku ps:restart` as backstop). Deps: T-402.
  Tests: tier 4. Agent: test. **Status: SCRIPTED 2026-11-03**
  (`39f104b`, kill/revive fingerprint exercise); live revival asserted
  against the restart policy + `dokku ps:restart`.
- **T-406 Self-deploy gated path.** Drift report + `topctl deploy
  core/top-controller` flow with failure preservation. Deps: T-304, T-402.
  AC: acceptance in M4 file. Tests: tier 4. Refs: ADR-0005. Risk: high by
  design → gated. Agent: core-go. **Status: DONE at tiers 1–2 2026-11-03**
  (`59633a6`, `3c3b23d`, `6f2b972`); cli.md contract amendment landed
  2026-11-04 (owner verdict R4).
- **T-407 M4 review + clean-VM repeat run.** Deps: all M4. Approval: owner.
  Agent: review. **Status: IN PROGRESS 2026-11-03** — review packet
  drafted (docs/milestones/m4-review.md); remaining: live e2e + soak run
  reports (concurrent session), then the clean-VM double run, then owner
  sign-off on decisions R1–R4 + the carried laws-CI decision.

## Milestone 5 — Operational hardening

- **T-504 Failure-injection pack.** Scripted matrix from testing-strategy
  (kill mid-deploy, reboot). Deps: T-304. AC: acceptance 7 + matrix green.
  Tests: tier 4. Agent: test.
  **Status: SCRIPTED 2026-11-03** (`8484589`, `a2adf33`);
  `test/e2e/t504-{kill-deploy,reboot}.sh` — live-run green across the
  matrix is the AC and remains pending.
- **T-505 Fresh-eyes contributor run.** External-style run of acceptance 10;
  fix all doc gaps found. Deps: docs. AC: checklist signed.
  **Approval: owner coordinates.** Agent: review + docs.
- **T-506 Docs completion + ADR acceptance sweep.** All runbooks final;
  ADRs → accepted; README status → v0. Deps: all M5. Agent: docs.
  **Status: IN PROGRESS 2026-11-03 (docs subagent)** — runbooks re-verified
  and finalized against the code (backups/reconcile-diff/
  installation/teardown, commit `8dfebe6`); contract code-truth
  alignments flagged with verdict ids (`a0893f6`); stale package
  READMEs fixed (`150cdac`); M5 milestone record added (`0b6c61a`);
  owner-verdict table consolidated below. ADR status sweep result: all
  15 ADRs carry `- Status: accepted (2026-10-27, owner approval)` in
  the file and the index. Remaining: T-505/T-507 ordering confirmation.
- **T-507 Release engineering.** goreleaser or equivalent: versioned
  binaries, checksums, install-top.sh pinning, CHANGELOG. Deps: T-506.
  AC: install from release artifact on clean VM. Agent: core-go.
- **T-508 v0 review + tag.** Approval: **owner.** Agent: review.
  **Status: DONE 2026-11-07** — v0.1.0 pushed to
  https://github.com/toledompm/top (tag on `main` at `67d361e`).

## Milestone 6 — Remote `topctl` (accepted 2026-11-07 with ADR-0016; unscheduled)

- **T-601 ssh transport in `internal/exec`.** Runner variant for a host
  target (`--host USER@HOST` / `TOPCTL_HOST`): BatchMode, ConnectTimeout,
  typed host-key refusal + loud `--dangerous-accept-new-host` opt-in,
  redaction parity with the exec canon. Deps: ADR-0016. AC: tier-1
  refusal/timeouts matrix. Tests: tier 1. Agent: core-go.
- **T-602 Verb threading.** Steady-state verbs (diff/doctor/deploy/
  history/logs/reconcile --now/uninstall) accept the host target; local
  mode unchanged and remains CI's mode. Deps: T-601. AC: M6 acceptance
  1, 3, 4. Tests: tier 1. Agent: core-go/cli-go.
- **T-603 tier-3 ssh-container twin.** sshd container + real verbs
  against a Dokku container; M6 acceptance 1+2+5 evidence. Deps: T-602.
  Tests: tier 3 (disposable). Agent: test.
- **T-600 Homelab bootstrap UX: repo + keys setup script (gh-assisted).**
  BEFORE M6 (prototype-feedback priority, owner 2026-11-13): a
  `scripts/bootstrap-repo.sh`-style flow that (a) creates the private
  platform repo via `gh repo create --private` (or local git+bare remote
  fallback), (b) generates the dedicated deploy key + known_hosts, (c)
  registers it as a deploy key via `gh api` (write-access flag toggled
  around `--init` automatically, then flipped read-only), and (d) for the
  steps that CANNOT be automated (browser-only pages, e.g. the webhook
  secret screen), prints a direct deep-link URL and blocks until the
  operator confirms the value is pasted back. Motivation: the first live
  homelab install (2026-11-13, media-server) surfaced this as the
  highest-friction step — manual key/page juggling ahead of `topctl
  install --init`. Deps: gh CLI on the operator's machine. Tests: tier 1
  (sh -n + fixture runs). Agent: cli-go.

- **T-604 Owner docs pass.** Remote ops runbook (homelab flow from the
  operator's machine) + contracts rows (`--host`). Deps: T-602. Agent:
  docs.

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

## M1.5 — owner-directed addition (appended 2026-10-27)

- **T-113 Bend formal laws (PROTOTYPE).** Machine-checked invariants over
  TOP's pure core in Bend v2.0.39: `laws/LAWS.bend` (human claims) +
  `PROOF.bend` (AI-maintained proofs) + `CODE.bend` reference model +
  `reference.bend` JSON entrypoints, deep-compared against
  `internal/plan.ComputeAll` by the Go differential harness (`test/bridge`,
  seeded random pass). Wire: `make laws` gate + AGENTS.md guidance + scope
  guard. Deps: M1 (T-110 plan engine, T-111 conformance seam). AC: proof
  gate prints ALL PROOFS CHECK; bridge deep-equal across hand tables +
  seeded randomized cases; default `make test` unaffected (graceful skip).
  Tests: proof gate + differential pass + negative controls (deliberate
  model divergence must fail). Refs: ADR-0015 (proposed), ADR-0012,
  laws/README.md. Risk: none (no prod code changes). **Approval: owner
  verdict on ADR-0015 pending.** Agent: core-go + test.

---

Post-v0 note: T-602 is unaffected by T-113; if ADR-0015 is accepted, more
laws (ordering acyclicity, ledger invariants) land in
M3–M5 review doc, never open at merge time.

## Upkeep (between milestones; spawned at owner request)

- **T-399 Dead-code + docs upkeep pass (post-M3).** Whole-repo sweep for:
  unused helpers/types/files (golangci-lint `unused` + manual review of
  exported-but-unreferenced surfaces), stub-era vestiges (e.g. `tools/tools.go`
  if the Makefile/AGENTS.md workflows no longer require its pins — verify how
  gofumpt/golangci-lint are actually invoked before removing), dead test
  helpers, README/doc drift vs code truth (package READMEs + contracts row
  spot-checks). Rules: every removal justified in the report (grep-verified
  zero references); tests stay green (full -race matrix + tier-3 NOT
  required for removals; conformance/laws/hygiene gates still run);
  fake/fixture code never counted dead while its harness is in flight.
  Deps: M3 review. AC: matrix green post-pass; a short removals table in
  the report; no behavior change. Tests: existing. Refs: ADR-0014, AGENTS.md
  §5. Risk: none. Approval: none (non-destructive; deletions revert via
  git). Agent: review + core-go.


## Fleet carry-overs (tier-4 e2e run, 2026-11-03)

- **F-1126 Poll→ForcePull for mutable-tag moves is not wired.** The M3 AC
  #6 adapter part is tier-3 proven (`TestE2EApplyMutableTagForce` in
  internal/backend/dokku/e2etest), but no caller ever sets
  `backend.ApplyOptions{ForcePull: true}` in production — the reconciler's
  image-source apply needs a poll-level "tag moved" signal (digest delta
  probe) before the acceptance closes. Tests: tier 3/4. Refs:
  docs/milestones/m3-gitops-controller.md AC 6. Agent: core-go.
- **F-1127 Secret checksums must reach Compute.** The plan's
  changeSecretRefs "unresolvable ⇒ re-set" branch never converges because
  ObservedState.SecretChecksums is never populated (apply-time store
  record + inspect read; README gap named in plan.go). Until then
  `topctl diff` never reads clean on secretRef specs. Tests: tier 1/4.
  Refs: T-403. Agent: core-go.
- **F-1128 Install should wire the controller's TOP_GIT_URL.** Nothing in
  the install path sets the bootstrap app's git env (operator config:set
  needed today). Tests: tier 4. Refs: T-402. Agent: core-go.

## Owner verdicts required — consolidated (T-506 sweep, 2026-11-03)

One table for the owner pass (rule 3: verdicts land as ADR/contract
amendments). R1–R4 are M4's list (docs/milestones/m4-review.md §4).
Nothing is pre-blessed.

| # | Question (one line) | Where the code/contract stands |
| --- | --- | --- |
| R1 | Add `spec.args[]` to the ServiceSpec contract (prometheus retention flags; fingerprint-covered redeploy)? | **RESOLVED 2026-11-04: APPROVED** — service-spec.md amended; implementation = T-411 |
| R2 | Add `spec.storage[].readOnly` (read-only host binds for node-exporter/cAdvisor)? | **RESOLVED 2026-11-04: APPROVED** (fail-safe pre-adapter; WARN-op interim) — service-spec.md amended; implementation = T-412 |
| R3 | Bless installer.md phase-5 wording fix ("bootstrapped controller loads the repo" → composition-root clause) + ADR-0005 amendment putting `adoptedByOp`/`firstRunOp` on the op record and naming `store.MarkManaged` | **RESOLVED 2026-11-04: APPROVED** — installer.md phase 5 + ADR-0005 amendment 2026-11-04 |
| R4 | Bless the cli.md deploy-row amendment (the `core/top-controller` confirm-gated exception) | **RESOLVED 2026-11-04: APPROVED** — cli.md row finalised |

Carried from M3/M4 (unchanged): T-399 upkeep candidates; the
`topctl secrets` verb backlog;
~~the `make laws` CI REQUIRED-vs-optional decision~~ **resolved
2026-11-04 — REQUIRED** (`.github/workflows/ci.yml`, ADR-0015 amendment);
~~acceptance-2 script-vs-run-report artifact decision~~ **resolved
2026-11-04 — the run report is the artifact** (m4-review.md §4).

## Owner-verdict implementation tasks (approved 2026-11-04, not yet built)

- **T-411 `spec.args[]` (R1).** Make the newly blessed ServiceSpec field
  real: `internal/spec` parse + strict validation (repeated
  `args: []string`, default `[]`, entries must be strings, KnownFields
  stays strict); `internal/plan` fingerprint/diff so an arg-only spec
  change fingerprints as a whole-service change and emits an update row
  (redeploy — never a silent no-op); backend adapter wiring so the args
  reach the container entrypoint on apply (image and build service paths
  alike); `internal/domain` vocabulary + `internal/install/platformrepo`
  template specs + README learn the field. Deps: T-103, T-110, T-305.
  AC: arg-only change redeploys exactly once, next plan is empty
  (idempotency rule), contract examples + fixture specs stay valid,
  fingerprint golden covers args-vs-env distinction. Tests: tier 1
  (+ tier 3 apply row). Refs: service-spec.md (R1 rows/rules),
  platformrepo/README.md §145–156. Risk: low. Agent: core-go.
- **T-412 `spec.storage[].readOnly` (R2).** Same treatment for the
  read-only bind field: `internal/spec` parse + validate (bool, default
  `false`); `internal/plan` diff rows for it; adapter wiring is
  **two-phase by design**: when Dokku gains a mounted-read-only flag, use
  it on apply; until then spec validation PERMITS `readOnly: true`
  (fail-safe — not a parse error) and the adapter emits a WARN op detail
  documenting the manual `docker-options:add <app> ...:ro` interim (the
  T-403 template-note convention), keeping node-exporter/cAdvisor metric
  jobs PROVISIONAL (README §183–191). Deps: T-103, T-110, T-305.
  AC: diff shows readOnly changes; `readOnly: true` spec validates +
  applies with the WARN detail on the op record; no behavior change when
  false/absent. Tests: tier 1. Refs: service-spec.md (R2 rows/rules),
  platformrepo/README.md §183–191. Risk: low. Agent: core-go.
