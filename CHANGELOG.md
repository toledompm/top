# Changelog

Notable, user-facing changes to TOP and `topctl`. Format follows the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions; this
file is human-owned — goreleaser emits machine-generated per-release notes
from commit subjects, and `CHANGELOG.md` is curated per release. Versions
follow [SemVer](https://semver.org) (tags: `v0.X.Y`).

## [Unreleased]

## [v0.1.1] — 2026-11-14 (live-homelab fixes)
### Fixed
- Install phase 0's repo probe rides the deploy-key transport (bare ssh
  under sudo reported a readable repo unreachable).
- `DeployImage` classifies dokku's no-changes `git:from-image` (exit 1,
  "No changes detected" marker on either stream) as already-current
  success — the bootstrap resume path re-runs an identical deploy.
- Install failures carry a bounded stderr excerpt in the error + op
  record; doctor failure summaries name the failing checks.
- Bootstrap provisions the deploy key under gitsource's ADR-0008 name
  (`id_ed25519`), and the CLI run-input paths wire `VolumeRoot`
  (`<dataDir>/volumes`) + `ProvisionSrc` so storage-bearing specs and
  config sync work on the first reconcile.
- Controller image is now a real release artifact
  (`ghcr.io/toledompm/top/topctl:vX.Y.Z` with the `v` prefix intact),
  published by CI on every tag; the fictional placeholder default and
  the `bootstrap-pinned` rolling alias are gone (T-605 for the
  observability fleet: real upstream version tags).

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
