# M5 review packet — backups, recovery, hardening (T-501..T-507)

## Status

**Implementation complete; sign-off owner actions pending.** All seven
implementation tasks landed in green, rule-7 units; evidence table below.
Published runbooks + backlog carry the merged owner-verdict table (R1–R8,
`docs/tasks/backlog.md` §"Owner verdicts required"); T-505 (fresh-eyes) and
T-508 (v0 tag) are owner-coordinated endpoints.

## Gate matrix (2026-11-03, orchestrator)

- `go build ./...`, `go vet ./...` — clean.
- gofumpt `mvdan.cc/gofumpt@v0.7.0 -l .` — empty.
- golangci-lint `v2.1.6 run` — 0 issues.
- `go test ./... -race -count=1` — **23/23 packages ok** (+1 vs M4: recover test file).
- `./test/hygiene.sh` — passed.
- `make release-check` skipped gracefully without an `origin` remote; the
  recipe was verified live by the T-507 agent (snapshot build → 2 binaries
  + checksums.txt, no warnings).

## Acceptance table

| M5 deliverable / task | Status | Evidence |
| --- | --- | --- |
| T-501 backup engine (create/verify/prune + registry + metrics) | DONE (tier 1) | `internal/backup/{backup.go,prune.go}` per ADR-0007 (VACUUM INTO, watermark-scoped, 14d/8w retention); store surface `RemoveBackup` (`98ac1f7`); engine metrics wired (`d1af6f0`); 24 hermetic tests |
| T-502 offsite copy (rsync/SSH) | DONE (tier 1) | `internal/backup/offsite.go` (argv-whitelisted, BatchMode ssh, no-secret-logging tested); destination-down never fails local backup; `top_backup_offsite_*` metrics (`58405d3`) |
| T-503 recover suite (restore + safeguards) | DONE (tier 1) | `internal/recover/restore.go` (ADR-0007 order, safety snapshot, `--force`, offsite lifeline via `Offsite.Pull`); safeguards `TestRestoreRefusesOlderWatermark`, `TestRestoreSafetySnapshotPrecedesMutation` + 11 further refusal-matrix tests (`7e7dead`, `7d45808`) |
| T-504 failure-injection matrix (4 rows) | SCRIPTED (tier 4, not live-run) | `test/e2e/t504-{kill-deploy,wedged-db,disk-pressure,reboot}.sh` + hermetic ENOSPC shard `internal/backup/diskpressure_test.go` (`8484589`..`7ff71eb`, `f96cc5f`) |
| T-505 fresh-eyes run | NOT STARTED | owner-coordinated (backlog) |
| T-506 docs + ADR sweep | DONE | runbooks corrected to actual behavior incl. `[CLI pending]` stub accuracy (`8dfebe6`), contract alignments (`a0893f6`, R-table), `m5-backups-recovery-hardening.md` record (`0b6c61a`), README v0 posture (`42f4ae1`); all 15 ADRs already formally accepted in-file |
| T-507 release engineering | DONE (verified local smoke) | `.goreleaser.yaml` (v2.19.0 pin noted in header; go1.27 toolchain auto-switch documented), version `-X main.version` → `cmd/topctl/main.go:15` matching `top_build_info`; fail-closed `TOPCTL_VERSION_PIN` checksum verify in `install-top.sh`; tags-only `release-dry.yml`; owner procedure `scripts/release.md`; `CHANGELOG.md` (`6ba0d1e`, `b3ec9b6`, `d46e0a3`, `f49d486`) |
| Exhaustion/observe items carried | OPEN | soak report + live tier-4 matrix greens flagged below |

## Deviations / proposals (owner verdicts required)

Full consolidated table with one-line rationale per item is in
`docs/tasks/backlog.md` §"Owner verdicts required" — R1–R4 from M4,
R5 (backup schedules: daily-jobs seam vs caller-invoked),
R6 (offsite rsync choice + `CopiedOffsiteAt` registry marker store amendment),
R7 (restore-mechanics triple: no post-restore op record / file-based admission /
safety snapshot), R8 (backup CLI verb wiring pre- vs post-v0).
Notable corrections made during implementation:

- ADR-0002's `TOP_ARCHIVE_SHA256` is an adapter archive-key, **not** release
  naming canon — `.goreleaser.yaml` header records this so nobody re-litigates.
- T-504 wedged-DB row wedges the sqlite **file**, not the dir it shares with
  secrets (documented in the script header).
- Backup CLI verbs remain `notImpl` (`internal/cli/stubs.go:736/:747/:756`);
  the ENOSPC scripted row of the disk-pressure matrix stays gated on R8.

## What remains before v0 sign-off (T-508)

1. **Owner verdicts** R1–R8 (+ carried: laws-CI required-vs-optional, T-399
   upkeep candidates `IsErrLockedLease`/`FakeRunner.Reset`/go.mod tidy).
2. **Live evidence remaining**: one disposable-VM fleet pass for the tier-4
   scripts (`t404`, `t405`, `t504-*`, host procedures in each script header),
   the M4 1h soak closing report (acceptance 7 sign-off precondition), and
   the 1-tier ssh-container offsite script (specced, uncommitted).
3. **Clean-VM double run** (install → converge → recover), then M4+M5
   acceptance green end-to-end twice consecutively.
4. **T-505** fresh-eyes contributor run; **T-508** review + tag via
   `scripts/release.md`.
