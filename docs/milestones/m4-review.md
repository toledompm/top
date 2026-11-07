// SPDX-License-Identifier: MIT

# T-407 — M4 review packet (self-managed core & observability)

Prepared as a docs-only review artifact (2026-11-03). Nothing outside
`docs/` was touched; the live e2e fleet (clean-VM runs, soak) runs in an
an accompanying session — every acceptance row that only a real VM can
prove is marked **pending live-run results**, never claimed. This packet
mimics the M3 shape (`26c499e`, T-311 record in
`docs/milestones/m3-gitops-controller.md`): gate matrix, acceptance table
with cited tests, amendment/deviation list, open items, and what remains
for the owner.

## Fleet result

Task → commits (backlog §Milestone-4):

| Task | Commit(s) | What landed |
| --- | --- | --- |
| T-401Example platform repo | `ecd5392`, `7c346bd` | `internal/install/platformrepo` (= `examples/platform-repo`, symlink instance): 5 ServiceSpecs + `prometheus-config/` + `grafana-provisioning/`, CI-validated, README explains every field |
| T-402 Adopt-bootstrap reconciliation | `fba65c4`, `82c7e30`, `145aa40` | reconciler adoption-aware controller-self posture; installer phases renumbered to contract 0–6 with phase 5 first-reconcile + `MarkManaged` handoff; 8 adopt tests incl. tier-1 real pipeline |
| T-403 Observability wiring | `3ffd717`, `8d602b6` | template targets/datasource/provisioning entries + secret dash-kebab name; reconciler provisioning sync (`provision.go`, copy-sync) + `secretenv.go` dash-kebab mapping |
| T-404 E2E change→webhook→converged; missed→poll | `6d487a3` | `test/e2e/t404-webhook-poll.sh` + hermetic `lib.sh`/`lib_test.go` (tier 1, runs every PR) |
| T-405 E2E TOP-down survival | `39f104b` | kill/revive exercise (kill the controller, zero-redeploy fingerprint proof, poll backstop); the tier-3 restart-policy checks + a fleet live run carry it |
| T-406 Self-deploy gated path | `59633a6`, `3c3b23d`, `6f2b972` | `RunInput.SelfDeployApproved` surgical field + validateRun actor guard; CLI `firstrun.go` phase-5 handoff + `deploy core/top-controller` confirm gate; README deltas |

## 1. Gate matrix (2026-11-03)

| Gate | Result | Evidence |
| --- | --- | --- |
| `go build ./...` | GREEN | run 2026-11-03, no output |
| `go vet ./...` | GREEN | run 2026-11-03, no output |
| gofumpt | GREEN (0 unformatted files) | `gofumpt -l .` empty, 2026-11-03 |
| golangci-lint | **Run locally 2026-11-03**: `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.1.6 run` — 0 issues | `.github/workflows/ci.yml:41` runs `golangci-lint-action@v7` redundantly; correction to this packet's original note, which cited `make lint` (it skips when the CI-style tool invocation isn't found) |
| `go test ./... -race -count=1` | GREEN — 21 test packages ok, 0 FAIL/SKIP, **427 top-level test functions** (647 RUN blocks incl. subtests) | run 2026-11-03, `-count=1` |
| `make laws` | Go differential bridge GREEN (`./test/bridge` 7/7); Bend proof gate **graceful-skipped here** (`bend` not installed on this host — the Makefile's documented default) | run 2026-11-03 |
| ADR-0014 hygiene | GREEN | `./test/hygiene.sh` passed 2026-11-03 |
| Tier-1 e2e helpers | GREEN inside the matrix | `test/e2e` 7 tests (`lib_test.go`: `TestInjectEnvProbe`:61, `TestWaitUntil`:146, `TestHmacSha256HexMatchesGo`:185, `TestDockerRunningName`:229, …) |

Per-package top-level test counts (from the `-race -v -count=1` run):

| Package | Tests | | Package | Tests |
| --- | --- | --- | --- | --- |
| internal/backend/dokku | 81 | | internal/plan | 13 |
| internal/cli | 51 | | internal/reconcile | 26 |
| internal/config | 11 | | internal/secrets | 13 |
| internal/controller | 18 | | internal/spec | 14 |
| internal/doctor | 17 | | internal/store/sqlite | 16 |
| internal/domain | 31 | | internal/webhook | 9 |
| internal/exec | 11 |
| internal/gitsource | 19 | | test/bridge | 7 |
| internal/install | 43 | | test/e2e | 7 |
| internal/inventory | 10 | | test/fakeexec | 5 |
| internal/obs | 10 | | **Total** | **427** |

(`internal/store`, `internal/backend`, `cmd/topctl`, `conformance_check`,
`internal/exec/exectest{,/cmd/*}` have no test files — vet/build-covered.)

## 2. Acceptance table (milestone §Deliverables / §Acceptance)

Evidence = file:line of the test or script, or the commit. Rows that only
a real VM can prove are deferred to the run reports, per the packet's
no-claim discipline.

### Deliverables

| Deliverable (M4 §Deliverables) | Status | Evidence |
| --- | --- | --- |
| Example platform repo: publishable fictional specs for `core/top-controller`, `observability/*`; provisioned Grafana dashboards/datasources; small-host defaults | **DONE (code+README)** | `internal/install/platformrepo/README.md` (field-by-field §51–115, small-host defaults table §99); specs validated at tier 1: `platformrepo_validate_test.go` `TestTemplateFleetIsCompleteAndValid`:64 (9 files: README+5 specs+prometheus.yml+3 provisioning), `TestTemplateFleetIsFullyDigestPinned`:96, `TestTemplatePlaceholderDiscipline`:256 |
| Installer phase 5 (first reconciliation adopts bootstrap deployment; ledger conversion `bootstrap-owned → managed`) | **DONE at tier 1/2** | `internal/install/adopt.go` (guard→`MarkManaged` flip:156→one `FirstRunner` pass); `adopt_test.go` 8 tests: handoff+one-run `TestInstallPhase5AdoptsAndRunsFirstReconcile`:90, idempotency `TestInstallAdoptionIdempotent`:159, no-spec→info + redo `TestInstallAdoptionNoSpecStaysBootstrapOwned`:214, non-green/handoff-error `TestInstallAdoptionFailureStops`:261 / `:303`, refusals `TestAdoptRefusalsDirect`:328, real pipeline `TestInstallAdoptionRealPipeline`:598 (proves exactly ONE controller `git:from-image` from phase 4, zero convergence verbs). Reconciler side: `internal/reconcile/adopt.go` `adoptedSelf` posture; `TestAdoptSelfNoRedeployWhenSpecMatches` |
| Observability wiring: `top-internal` attachments, scrape config rendered from specs, Grafana admin via `secretRefs` | **DONE at tier 1** (VM scrape proof pending) | network attach in the reconciler field bundle (`apply.go` networks); targets wired: `TestTemplateObservabilityWiringIsFilled`:174 asserts the four alias targets (`…web:8080/9100/8080/3000`, :235–241) and datasource `http://observability-prometheus.web:9090` (:246); secretRef ≠ inline + dash-kebab rule: `TestTemplateGrafanaSecretRefNotInline`:142; reconciler mapping `internal/reconcile/secretenv.go` + `TestSecretEnvKeyDashKebabRule` (`provision_test.go`:196) |
| Controller metrics dashboard JSON (provisioned) | **DONE** | `internal/install/platformrepo/observability/grafana-provisioning/` (provider.yaml + dashboards), synced into one provisioning tree per `TestTemplateObservabilityWiringIsFilledIs…` sanity (single-entry assertion :171–179) — dashboard RENDERING live is a soak/VM check |
| E2E scripts for acceptance tests 2, 3, 4, 8 | **SCRIPTED; not live-run** | acceptance 3+4: `test/e2e/t404-webhook-poll.sh` (commit `6d487a3`); acceptance 8: restart-policy checks in the tier-3 harness + the fleet live run (commit `39f104b`); acceptance 2 (install green, docs-verbatim) is the harness's precondition, not separately scripted — see open items §4 |

### Acceptance criteria (maps to global tests 2, 3, 4, 8)

| # | M4 acceptance criterion | Status | Evidence / what a live run must show |
| --- | --- | --- | --- |
| 1 | Clean-VM install per public docs → `core` + `observability` reconciled green; Grafana reachable per routing mode | **PENDING live-run results — see soak/e2e run reports** | tier-1 composition guarantees exist (`TestInstallAdoptionRealPipeline`), but the clean-VM proof (and README verbatim check) must come from the run report |
| 2 | Git change to an observability spec → webhook → converged; same via poll with webhook dropped | **SCRIPTED, PENDING live-run** | `test/e2e/t404-webhook-poll.sh` (actor=webhook op record + dokku deploy evidence + file:line drift origin; poll-repair with NO webhook evidence on the record). Tier-1 helpers green (`lib_test.go`, incl. the HMAC byte-equality with `internal/webhook.verify`:185). Handler math long-covered (`internal/webhook` 9 tests) |
| 3 | `docker kill` controller + `dokku ps:stop`: Prometheus/Grafana/exporters/sample app keep running; controller returns (restart policy, `dokku ps:restart` as backstop); missed changes reconcile on return | **PENDING live-run evidence** | restart-policy checks in the tier-3 harness + the fleet live run (docker-name fingerprint stability ⇒ never re-ran `git:from-image`; adoption stays one-shot). `restartPolicy: always` asserting at spec level is documented (`platformrepo/README.md:93`) |
| 4 | `topctl status` shows drift on `core-top-controller` on spec-revision advance; `topctl deploy core/top-controller` applies it (human-gated) with old-release preservation | **PARTIAL — self-deploy path DONE at tier 1/2; `status` NOT delivered** | deploy: `SelfDeployApproved` guard `adopt_selfgate_test.go` (`TestSelfDeployApprovedHumanApplies`:56, `TestSelfDeployWithoutApprovalStaysReportOnly`:92, `TestSelfDeployApprovalNeedsManagedRow`:112, `TestSelfDeployRejectedForNonUserActor`:135); CLI gate matrix `deploy_self_test.go` (`TestDeploySelfHumanGateMatrix`:130: non-TTY→2-and-zero-verbs, `--confirm`→`git:from-image` `v2`+echoes, TTY `y` same; `TestDeploySelfFailureKeepsOldRelease`:179: verb fails→exit 3, nothing destructive). **`topctl status` remains the M1 validated stub** (`internal/cli/README.md:7`) — the drift-report half of this row is NOT satisfied by `status` (see deviations: `topctl diff` carries it today) |

### Failure scenarios (M4 §Failure scenarios — soak/VM proof pending all)

| Scenario | Status | Note |
| --- | --- | --- |
| Grafana down ⇏ blindness (`topctl doctor` functional) | PENDING live-run | code shape supported (doctor reads ledger/store, not Grafana) but unproven live |
| Prometheus spec broken → contained reconcile failure, controller unaffected | PENDING live-run | per-service containment is tier-1-proven (`internal/reconcile` fake-backend outcome propagation) |
| Controller self-deploy to broken image → checks fail, old controller keeps running, op failed | TIER-1-PROVEN shape; live pending | `TestDeploySelfFailureKeepsOldRelease`:179 (verb failure → exit 3, no destructive verb); real-Dokku failed-check preservation for ordinary apps is M3-proven (`TestE2EApplyFailingCheckPreservesOldRelease`), self-app variant pending VM |

## 3. Amendments / deviations from the literature (PROPOSALS unless owner-blessed)

All of these were recorded in the wave reports/commit messages and the
package READMEs; none has a committed contract/ADR change yet. AGENTS.md
rule 3: durable decisions belong in ADRs — the owner verdict below (§4)
turns each into one.

1. **ADR-0005 deviation — adopted-by-op annotation on the op record, not
   the ledger row.** `MarkManaged` stays a pure state conversion; the
   provenance (`adoptedByOp` + `firstRunOp` op ids) lives in the phase-5
   operation record details (`internal/install/adopt.go:40–41,141,160`),
   so **zero store-surface changes** were needed. The M4 plan implied
   ledger annotation. Needs an ADR-0005 one-line amendment.
2. **Phase renumbering 5→first-reconcile + new phase 6.** Adopted in
   `82c7e30`; **the contract already carries it** —
   `docs/contracts/installer.md:37–44` has phase 5 First reconciliation
   (adopt + `managed` conversion, exit 0 on green `Readiness(core)` +
   recorded first run) and phase 6 Report. *Residual gap:* the contract
   text says "the **bootstrapped controller** loads the repo" (installer.md:37)
   while the implementation runs the phase-5 pass **from the install CLI**
   via the `Options.FirstRun` seam (`internal/cli/firstrun.go`;
   `internal/install/adopt.go` doc), with the adopted controller only
   taking over afterwards. One sentence, owner-blessed, would fix that
   drift (same ADR-0005 amendment as item 1).
3. **T-403 deviations (all documented with mechanisms + operator
   interims in `internal/install/platformrepo/README.md:117–204`):**
   - **Retention flags not wired** — the v0 spec cannot express container
     args; proposal: add `spec.args[]` (README §145–156; needs spec +
     domain + plan + adapter alignment). Owner decision **R1**.
   - **Exporter host mounts not wired** (`/proc`, `/sys`, docker.sock
     read-only binds) — no combined Dokku read-only-mount surface exists;
     proposal: `spec.storage[].readOnly` (README §183–191); node-exporter
     and cAdvisor metric jobs are PROVISIONAL (container-local numbers)
     until then. Owner decision **R2**.
   - **`secretRef` → env mapping is dash-kebab**, applied only on the
     reconciler seam (`secretenv.go`); plan/vocabulary keep naming
     `secretRefs.<name>` — no contract text contradicts it. **Batched
     Apply semantics remain verbatim-keyed** (unchanged from M3).
   - **Provisioning delivery mechanism chosen: COPY-SYNC**, not configmaps:
     each reconcile copies `prometheus-config/` and `grafana-provisioning/`
     into the TOP-owned VolumeRoot entry dir (hash-diff, stale-pruned,
     BEFORE any apply mounts) — `internal/reconcile/provision.go`, tests
     `provision_test.go`= `TestProvisioningSyncConvergesTree`:34,
     `TestProvisioningSyncPrunesStaleFiles`:86,
     `TestProvisioningSyncSkipsDataAndUnaware`:129,
     `TestProvisioningSyncSkipsFileTreeLeaf`:166. Reason: gitsource
     materializes revisions into ephemeral temp dirs; only a stable
     copied path can back a Dokku bind mount (README §135–144).
   - **Scrape aliases = Dokku network alias scheme `<app>.<proc-type>`**
     (`core-top-controller.web:8080`, …), docs-verified against Dokku's
     network-management docs (README §157–165), pinned at tier 1
     (`platformrepo_validate_test.go:225–244`). Aliases exist only
     attach-post-deploy → a `down` target means failing checks.
4. **T-406 deviations:**
   - **`RunInput.SelfDeployApproved` surgically added** (one field, not a
     posture rewrite); validateRun refuses it from any non-`user` actor
     (`TestSelfDeployRejectedForNonUserActor`:135) and it cannot bypass
     the pre-adoption posture (`TestSelfDeployApprovalNeedsManagedRow`:112).
   - **`TestInstallCLIFullRunEnvelope` converted to the file://-fixture +
     split-runner pattern** (`internal/cli/install_test.go:143–166`)
     because the pure-fake ssh row cannot satisfy a real gitsource fetch.
   - **Self-deploy echo is text-mode only** (`—` skipped when `-o json`;
     the JSON envelope already carries fields): `internal/cli/stubs.go:434–437`.
   - **Confirm-gate has no persisted audit artifact** — the echo prevails
     (printed on success AND failure, `deploy_self_test.go:184–187`); the
     confirm knot remains validation-time-only (`internal/cli/README.md:64–79`),
     the artifact gap is open for the same owner pass as item 1.
   - **`docs/contracts/cli.md` line 18 is stale**: the `deploy` row says
     "not confirm-gated" with no exception for `core/top-controller`
     (which IS confirm-gated per `internal/cli/README.md:129–138` and
     `stubs.go:431–433`). Needs a one-line, owner-blessed contract
     amendment (T-406's text-mode echo, exit-2 non-TTY row is the exact shape).
5. **Run report so far**: adopt/t404 reuseable scripts are SHIPPED
   (`test/e2e/README.md:36–52` documents VM preconditions + env contract)
   but NOT live-run in this packet's scope — every row above marked
   pending alone on the run reports.

## 4. Open items (owner decisions + follow-ups)

Owner decisions pending (add these to the decisions table when resolved):

- **R1** — add `spec.args[]` to the ServiceSpec contract (prometheus
  retention flags on the small host; fingerprint-covered so arg-only
  changes redeploy). **RESOLVED 2026-11-04: APPROVED** — service-spec.md
  amended; implementation task T-411 (backlog).
- **R2** — add `spec.storage[].readOnly` (read-only host binds for
  node-exporter/cAdvisor; also constrains Dokku `storage:mount` to
  read-only consumers once Dokku grows the flag). **RESOLVED
  2026-11-04: APPROVED** (fail-safe pre-adapter, WARN-op interim) —
  service-spec.md amended; implementation task T-412 (backlog).
- **R3** — bless the installer phase-5 wording fix ("bootstrapped
  controller" → composition root clause) + ADR-0005 amendment putting
  `adoptedByOp`/`firstRunOp` on the op record and naming `store.MarkManaged`.
  **RESOLVED 2026-11-04: APPROVED** — installer.md phase 5 + ADR-0005
  amendment 2026-11-04 landed.
- **R4** — bless the `docs/contracts/cli.md` deploy-row amendment (the
  `core/top-controller` confirm-gated exception). **RESOLVED
  2026-11-04: APPROVED** — cli.md row finalised.
- Carried from M3: the `make laws` CI job REQUIRED-vs-optional decision
  (M3 review, T-311). **RESOLVED 2026-11-04: REQUIRED** — ci.yml laws
  job is a non-optional gate; ADR-0015 amendment landed.
- T-399 upkeep candidates still queued: `IsErrLockedLease`,
  `FakeRunner.Reset`, `go.mod tidy`.
- `topctl secrets` verb backlog (the operator setup step until then is
  README-documented, §199–204; also unlocks secret-name discovery).
- **`topctl status` is still a stub** — M4 acceptance row 4 names it;
  either scope a real drift view into M5 or amend the row to `topctl diff`.
- Acceptance-script coverage for M4 acceptance 2 (install green per
  public docs on a clean VM): **RESOLVED 2026-11-04 (owner verdict)** —
  ACCEPT THE RUN REPORT as the artifact; NO t402-style script is
  committed. Rationale: scripts add no value here — the install-green
  precondition is exercised live by every fleet run, and
  the live run report + gate matrix + the instrumented fleet scripts
  already cover the operational surface; a t402 script would duplicate
  the precondition without adding evidence.

Risks (from M4 §Risks): small-host OOM from the stack → R1/R2 + resource
limits are the planned knobs; rollback remains "scale down specs via Git"
(dogfood proof belongs to a later step).

## 5. What remains before owner sign-off

1. **Live e2e + soak run reports** — every row above marked "pending
   live-run results" closes here: clean-VM install (M4 acceptance 2),
   webhook/poll convergence (3+4, the fleet scripts), TOP-down survival (8,
   the restart-policy checks + live run).
   These run in the concurrent session; DO NOT double-run against a
   shared VM.
2. **Then the clean-VM double run** (M4 §Definition of done: acceptance
   green end-to-end from a clean VM twice in a row) — the repeat run is
   also T-407's own "clean-VM repeat run" deliverable.
3. Owner verdicts on R1–R4 (+ the carried laws-CI decision), which land
   as ADR/contract amendments per rule 3. **DONE 2026-11-04** — R1–R4
   approved + laws job REQUIRED; amendments in cli.md / service-spec.md /
   installer.md / ADR-0005 / ADR-0015 / ci.yml; backlog adds
   T-411/T-412.
4. Backlog closure: mark T-401..T-407 done after sign-off.

## Status addendum 2026-11-04 (owner verdicts)

- **R1–R4 APPROVED; carried M3 laws-CI decision RESOLVED (the laws job is
  REQUIRED); M4 acceptance 2 KEEP THE RUN REPORT as the artifact — no
  t402-style script is committed** (rationale: scripts add no value here;
  the live run report + gate matrix + the instrumented fleet scripts
  cover the operational surface; install-green is every
  fleet run's precondition). Contract/ADR/workflow amendments landed
  2026-11-04; implementation tasks T-411 (`spec.args[]`) and T-412
  (`spec.storage[].readOnly`) queued in the backlog. R5–R8 and T-399
  upkeep candidates remain open.

## Status addendum 2026-11-03 (orchestrator)

- **Live fleet units 1–4 ran GREEN** (tier-3 8/8 + mutable-tag hook, install/
  adoption invariant, `t404` webhook+poll, kill/revive survival) in the disposable
  environment, with small adapter fixes committed per unit (see git log +
  commit `6d487a3`+ successors). Soak (unit 5) was still running when fleet
  work continued to M5 — **its closing report is a sign-off precondition for
  acceptance 7, not a code blocker**.
- The `t404` `diff clean` caveat persists by design: M3 secret-checksum gap
  + the `drifted(self)` human gate (already in §4).
- M4 status: **code complete; soak evidence + R1–R4 owner verdicts pending.**
  M5 opened 2026-11-03 per owner direction; M5 work shares the
  disposable-environment budget, not the sign-off gate.
