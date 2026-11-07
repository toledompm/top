# M5 review packet — operational hardening (T-504..T-507)

## Status

**Implementation complete; sign-off owner actions pending.** The
hardening-scope tasks landed in green, rule-7 units; evidence table below.
Published runbooks + backlog carry the owner-verdict table (R1–R4,
`docs/tasks/backlog.md` §"Owner verdicts required"); T-505 (fresh-eyes) and
T-508 (v0 tag) are owner-coordinated endpoints.

## Gate matrix (2026-11-07, orchestrator)

- `go build ./...`, `go vet ./...` — clean.
- gofumpt `mvdan.cc/gofumpt@v0.7.0 -l .` — empty.
- golangci-lint `v2.1.6 run` — 0 issues.
- `go test ./... -race -count=1` — all packages ok (the test-package count
  reflects the v0 surface after the M5 scope ruling below).
- `./test/hygiene.sh` — passed.
- `make release-check` skipped gracefully without an `origin` remote; the
  recipe was verified live by the T-507 agent (snapshot build → 2 binaries
  + checksums.txt, no warnings).

## Acceptance table

| M5 deliverable / task | Status | Evidence |
| --- | --- | --- |
| T-504 failure-injection matrix | SCRIPTED (tier 4, not live-run) | `test/e2e/t504-{kill-deploy,reboot}.sh` (`8484589`..`939e588`); revival asserted against the restart policy backed by `dokku ps:restart`, health through `topctl doctor` |
| T-505 fresh-eyes run | NOT STARTED | owner-coordinated (backlog) |
| T-506 docs + ADR sweep | DONE | runbooks corrected to actual behavior incl. `[CLI pending]` stub accuracy (`8dfebe6`), contract alignments (`a0893f6`, R-table), README v0 posture (`42f4ae1`); all 15 ADRs already formally accepted in-file |
| T-507 release engineering | DONE (verified local smoke) | `.goreleaser.yaml` (v2.19.0 pin noted in header; go1.27 toolchain auto-switch documented), version `-X main.version` → `cmd/topctl/main.go:15` matching `top_build_info`; fail-closed `TOPCTL_VERSION_PIN` checksum verify in `install-top.sh`; tags-only `release-dry.yml`; owner procedure `scripts/release.md`; `CHANGELOG.md` |
| Exhaustion/observe items carried | OPEN | owner-directed live evidence below |

## Scope note

The M5 scope is hardening + release engineering + the docs sweep. Backups
are the operator's job (ADR-0013): the runbook (`docs/operations/backups.md`)
carries the operator recipe; revival is Dokku-side (restart policy / `dokku
ps:restart` / re-running the installer) and TOP's metadata DB is
reconstructible by re-running operations — reconcile is idempotent. The
failure matrix's health checks read through `topctl doctor`.

## What remains before v0 sign-off (T-508)

1. ~~Owner verdicts~~ R1–R4 resolved (see the backlog table) (+ carried:
   T-399 upkeep candidates `IsErrLockedLease`/`FakeRunner.Reset`/go.mod
   tidy).
2. **Owner-directed live evidence (2026-11-07)** — instead of the synthetic
   soak, the owner installs v0 on the homelab host directly from the
   published release; the exercise **replaces** the soak/soak-report item
   and the clean-VM double run as the acceptance evidence (live production
   install is the strongest form of both).
3. **T-505** fresh-eyes contributor run (post-v0 OK per owner);
   **T-508** review + tag via `scripts/release.md`.
