# Milestone 1 — Foundation

## Objective & scope

Stand up the Go module and engineering skeleton that everything else builds
on: contracts-as-code (interfaces + domain types), config, logging, exec
layer, SQLite store with migrations, CLI skeleton, and test seams (tiers 1–2
from ADR-0012).

## Non-goals

- Real Dokku calls (the adapter gets its interface + fake; real calls are M2/M3).
- Controller loops, webhooks, metrics endpoint (M3/M4).

## Prerequisites

- M0 approved. Go toolchain ≥ 1.24 (local and CI both go 1.24; deps were consciously pinned Go-1.24-compatible at T-101 — client_golang v1.23.2, sqlite v1.46.1, goose v3.26.0).

## Deliverables

- `cmd/topctl` with cobra commands as no-op/validated stubs; `-o json` plumbing
  with `schemaVersion`.
- `internal/domain`, `internal/spec` (strict YAML via `go.yaml.in/yaml/v3`,
  `KnownFields`), `internal/plan` (pure diff).
- `internal/exec` (`Runner` interface: argv-only, timeouts, WaitDelay, env
  hygiene, redaction) + `test/fakeexec` + dokku shim.
- `internal/store/sqlite` (modernc driver, DSN pragmas, goose migrations:
  schema v1 = operations/checkpoints/deliveries/ledger/backups/leases).
- `internal/config`, `internal/obs` (slog JSON + level var; registry helper).
- CI: tiers 1–2, lint/vet/fmt, ADR-0014 hygiene grep.
- Package READMEs; updated AGENTS.md if conventions changed.

## Acceptance criteria

- [ ] `go build ./...`, `go test ./...` green on Linux+macOS from a clean clone.
- [ ] ServiceSpec contract examples parse; invalid samples fail with file:line.
- [ ] Store round-trips an Operation through migrations from zero.
- [ ] Redaction unit tests prove registered secrets never reach captured output.
- [ ] CI passes on a fork with no secrets.

## Failure scenarios to test

- Corrupt YAML, unknown fields, both image+build, bad names.
- SQLite busy (two writers) → busy_timeout path.
- exec: timeout kill, non-zero exit, stderr-on-success, redaction.

## Documentation

- `docs/contracts/*` updated to match code realities; each package README.

## Risks & rollback

- Contract drift between docs and code → contracts compile-checked in tests.
  Rollback: none needed (no host impact).

## Definition of done

All criteria green in CI; backlog tasks M1-* closed; owner tag review.

## T-112 review (2026-10-27)

Full re-verification of the delivered M1 tree (go 1.24.5, `GOTOOLCHAIN=local`):
`make build vet test test-shim test-conformance hygiene` all green; `go test
./... -race -count=1` green across all 12 test-bearing packages; `gofumpt -l .`
clean; `golangci-lint v2.1.6 run` clean (0 issues); `go mod tidy -diff` clean
(the pre-T-112 "needs go mod tidy" note in internal/obs/README.md was stale and
has been removed).

### Contract verdicts

| Contract | Verdict | Evidence |
| --- | --- | --- |
| service-spec.md | **PASS** | internal/spec implements the field table verbatim (strict KnownFields, defaults per doc, file:line ParseErrors, mutable-ref warnings, reserved-namespace guard); contract fixtures parse; conformance probe idempotency over `testdata/valid`. |
| state-store.md | **PASS** | internal/store (interface) + store/sqlite: operations lifecycle, observed upsert, checkpoints (atomic run+checkpoint), delivery dedup, ledger, backup registry, leases with ErrLocked+holder, schema-version downgrade protection — probed by conformance_check. |
| platform-backend.md | **DEFERRED (planned)** | No `internal/backend` package yet — M2 (read slice, T-201) then M3 (Apply/Readiness, T-305/306). The M1 share is delivered: `exec.Runner` is the declared seam (backed by conformance compile-assert) and the tier-2 dokku shim exercises argv construction. Noted drift: contracts/README.md still says "final Go interfaces are derived … in Milestone 1"; the PlatformBackend interface itself is now an M2/T-201 artifact. |
| reconciler.md | **PARTIAL — diff step PASS, loop DEFERRED (planned)** | The pure diff step is fully implemented (internal/plan: Compute/ComputeAll, self-service report-only, prune-gated orphans, secret-safe rendering; probed). The pipeline (lease/order/apply/gate/record) is M3 (T-304) as planned. Every field in reconciler.md's diff-output section that plan v0 does not emit (env removal, healthcheck, secret rotation, routing.mode, storage size) traces to a dated M3 note in internal/plan/README.md's coverage matrix + "Known deviations", and to conformance_check's "Known gaps / M3 revisit" instruction. |
| git-source.md | **DEFERRED (planned)** | M3 (T-301/302/303). Nothing implements it; no drift — the CLI/config surface only carries `git.url`/`git.branch` placeholders. |
| backup-recovery.md | **DEFERRED (planned)** | M5 (T-501..T-503). The store-side prerequisites exist (backup registry incl. verification status, watermark fields, lease for restore exclusivity); the engine does not — as scheduled. |
| installer.md | **DEFERRED (planned)** | M2 (T-203..T-209). Stub-level: `topctl doctor/install/uninstall` exist as gated stubs with `--dry-run`/`--confirm` flags; phase wiring is M2. |
| cli.md | **PASS** (M1 slice) | All contract commands exist as validated stubs; envelope `schemaVersion: 1` pinned by a compile assert; exit codes 0–4 constants exist (stubs can produce 0/2/3; 1 and 4 are reserved for real data — as documented in internal/cli/README); every mutating stub has `--dry-run` + confirm gates. One open item: the `topctl secrets set` command named in ADR-0008 is in neither the cli.md contract table nor the stub tree (see below). |

### ADR / tracked-doc contradictions found (owner-visible amendments proposed, not applied)

1. **CI runs tier-1 only; ADR-0012 promises tiers 1–2 on every PR.**
   The `toptests`-tagged shim suite (T-105) is runnable root-free in ~2 s but
   is not invoked by `.github/workflows/ci.yml`, nor are the T-111
   conformance probes. The Makefile comment says the T-112 review decides
   when conformance joins CI. **Proposed amendment (ADR-0012 consequence
   note):** "shim-tier conformance under the `toptests` tag is invoked by
   `make test-shim` locally from M1; it joins PR CI post-M2 (T-202) once a
   shim-covered adapter exists to protect" — or simply add
   `make test-shim` (+ optionally `make test-conformance`) as CI steps now.
   Also `docs/milestones/m1-foundation.md` deliverables say "CI: tiers 1–2"
   — same deviation.
2. **`topctl secrets set` (ADR-0008) is not in cli.md nor implemented.**
   ADR-0008 says secret files are "managed out-of-band (operator writes
   them; `topctl secrets set <name>` helper reads from stdin…)". The stub
   tree has no `secrets` command. **Proposed amendment:** either add a
   `secrets set <name>` row to cli.md's command table (T-2xx/M2 wiring),
   or amend ADR-0008 to "managed out-of-band by the operator (stdin-fed
   helper documented in the install runbook) in v0".
3. **go-version claims in backlog/milestone text.** Backlog T-101 still
   (resolved: both files now read go 1.24 + pinned-compatible deps; reality is `go 1.24.0`
   toolchain go1.24.5 with prometheus `client_golang v1.23.2` (T-101 was
   consciously implemented at 1.24). **Proposed amendment:** backlog
   T-101 → "go.mod (go ≥1.24, pinned Go-1.24-compatible deps)"; m1
   prerequisites → "Go toolchain ≥ 1.24".
4. **ADR-0012 open question resolved but not closed.** "Lima vs Multipass
   … Confirm in Milestone 1" was resolved by owner decision #5 (Lima). No
   contradiction in substance; **proposed amendment:** mark the open
   question "confirmed: Lima (2026-10-27, owner decision #5)".
5. **internal/store/README.md is missing.** AGENTS.md requires every
   component to ship a README; internal/store (interface package) has only
   package-doc comments. Owner should authorize writing it (facts already
   gathered in this review's `go doc ./internal/store` run).

### Docs edited in this review

- `README.md` — status banner → "Milestone 1 (foundation) — in progress"
  with a one-line what-exists summary.
- `docs/contracts/service-spec.md:47` + `docs/milestones/m4-…:5` +
  `internal/spec/testdata/valid/…/prometheus.yaml:1-2` + the
  `test/hygiene-allowlist` note — "`top-homelab`-style" generalized to
  neutral "a platform repo" language (the hygiene exemption rule stays for
  any future hyphenated use).
- `internal/README.md` — created: package index matching ADR-0006, marking
  which packages exist (M1) and which land in M2/M3/M5.
- `Makefile` — added a `make help` target describing all targets.
- `internal/obs/README.md` — removed the stale "go.mod needs tidy" note.

### M1-capped acceptance-test status (12-test map, docs/architecture/testing-strategy.md)

| # | Test | Tier | M1 status |
| --- | --- | --- | --- |
| 1 | Install from repo per public docs | 4 | ✗ M2/M4 |
| 2 | Reconcile core + observability | 4 | ✗ M4 |
| 3 | Webhook change → reconcile | 3+4 | ✗ M3/M4 (store dedup primitive tested unit-level only) |
| 4 | Missed webhook → poll repairs | 4 | ✗ M4 |
| 5 | Manual diff/reconcile | 3+4 | ✗ M3 (the plan computation is implemented and probed; host apply is M3) |
| 6 | Invalid spec + failed-deploy safety | 1 + 4 | **½ tier-1 part green** (spec parser rejects all invalid classes with file:line — M1-row evidence); tier-4 deploy-safety half ✗ M3 |
| 7 | Controller restart mid-operation | 4 | ✗ M5 |
| 8 | TOP-down survival | 4 | ✗ M4 |
| 9 | Independent recovery | 4 | ✗ M5 |
| 10 | Backup + restore | 4 | ✗ M5 (registry/schema prerequisites delivered) |
| 11 | Uninstall spares foreign resources | 4 | ✗ M2/M5 (ledger primitives delivered; deletion rules unexercised) |
| 12 | Fresh-eyes contributor run | 4 | ✗ M5 |

Plus M1-only meta-verification: contract-conformance harness green
(compile asserts + tagged probes + doc-lint), tier-2 shim flow green,
race-clean, gofumpt/lint/hygiene clean.

### Residual risks (carry forward)

- **R-drift:** the reconciler.md env-removal/rotation/healthcheck gaps are
  explicitly deferred to M3 — the M3 conformance revisit instruction in
  conformance_check/README.md is the fence; do not lose it.
- **R-shim:** tier-2 shim is currently only locally exercised;
  a contributor-only PR never runs it (see amendment 1).
- **R-store-README:** internal/store interface package has no README;
  AGENTS.md requires one per component. Owner should authorize authoring
  it (small — the facts were gathered for this review).
- **R-env-canon:** env naming `TOP_DATADIR`|`TOP_DATA_DIR` is accepted
  (config honors both spellings); docs elsewhere (ADR-0012 testing) only
  use `TOP_*` names for test gating — no conflict, but pick one spelling
  in v0 docs if the CLI help ever advertises them.

### Owner open items

- License choice — still unchosen (repo has no LICENSE file).
- Bend-proposal — pending owner decision (carried from backlog).
- Accept/reject the four ADR amendment proposals above (ADR-0012 ×2,
  ADR-0008 secrets-set, backlog/milestone go-version lines).
- Authorize writing internal/store/README.md.
- Conformance/shim → PR-CI decision (recommended post-M2 for conformance,
  now for shim).

