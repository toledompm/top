# Changelog

Notable, user-facing changes to TOP and `topctl`. Format follows the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions; this
file is human-owned — goreleaser emits machine-generated per-release notes
from commit subjects, and `CHANGELOG.md` is curated per release. Versions
follow [SemVer](https://semver.org) (tags: `v0.X.Y`).

## [Unreleased]

## [M5 — T-507] — 2026-11-03 (development)
### Added
- Release engineering: `.goreleaser.yaml` (raw `topctl-<os>-<arch>` binaries
  + `checksums.txt`), `make release-dry-run` / `release-check` targets, a
  `release-dry` CI job on tag pushes, and `TOPCTL_VERSION_PIN` +
  checksums-pin support in `install-top.sh`.

## [M5 in flight — hardening]
- T-504 failure-injection pack (tier-4 scripts).

## [M4] — self-managed observability
- Observability stack: metrics (`top_build_info{version,commit}`, ...)
  wired via ADR-0010; live fleet verified (T-40x units).

## [M3] — GitOps controller
- Controller daemon + coalescing queue; reconcile engine per ADR-0002
  (level-based, sha-pinned): plan, diff, apply, verify.

## [M2] — bootstrap & host lifecycle
- Dokku adapter app CRUD + phased installer (T-205..T-208) with
  ownership-safe teardown per ADR-0009.

## [M1] — foundation (T-1xx)
- Go monorepo skeleton per ADR-0006: `internal/cli` topctl command tree,
  JSON envelope contract, SQLite state store, exec seam, Bend laws gate
  (ADR-0015).
