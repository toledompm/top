# Changelog

Notable, user-facing changes to TOP and `topctl`. Format follows the
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) conventions; this
file is human-owned — goreleaser emits machine-generated per-release notes
from commit subjects, and `CHANGELOG.md` is curated per release. Versions
follow [SemVer](https://semver.org) (tags: `v0.X.Y`).

## [Unreleased]

### Fixed
- Secret env keys under delegated Apply map through the dash-kebab rule
  (dokku rejected the raw service-name as an invalid config key), and
  resolved secret values are registered with the exec layer's redaction
  seed before any command carries them — a config:set failure no longer
  echoes the value.

## [v0.1.8] — 2026-11-15 (resources phantom + shared quantity vocabulary)

### Fixed
- Resource-limit comparison in the plan is semantic (K8s-style spec
  spellings vs dokku's stored docker-native ones mean the same limits):
  the literal compare re-proposed a resources change on every run,
  keeping every plan non-empty so converged services re-ran Apply —
  and on the live fleet that re-drove deploys that should never have
  been attempted. The quantity translation moved to `internal/domain`
  as shared vocabulary for both apply paths.

## [v0.1.7] — 2026-11-15 (failure-report tail fix)

### Fixed
- The archive-deploy failure excerpt now takes the TAIL of dokku's
  output: the head-excerpt spent its entire budget on the successful
  boilerplate (tar fetch, git update, build header) and cut right
  before buildkit's actual error line, rendering three live failures
  with no visible reason.

## [v0.1.6] — 2026-11-15 (archive-deploy readiness fix)

### Fixed
- Archive-deployed apps (the checks-carrying `git:from-archive` path)
  now pass the reconcile readiness gate on the archive fingerprint's
  presence instead of an image reference dokku never records for tar
  deploys — the cadvisor conversion on the live fleet deployed fine and
  then failed readiness for its whole 3m deadline on `image=""`.

## [v0.1.5] — 2026-11-15 (converge unification + port exposure)

### Fixed
- Port exposure follows `routing.mode` (`internal/plan`): internal-mode
  services no longer converge to host nginx listeners (media-server
  2026-11-14: listeners on contested host ports 8080/9090 made every
  nginx config reload abort, so nothing spec-driven went live). An
  unpinned `hostPort` now compares equal to dokku's host==container
  default, ending the phantom per-run port diff on every default-port
  app. `ledgerApp` records EVERY spec service (converged apps included)
  — the ledger row is the prune gate's ownership key.
- Failed deploys carry a stdout excerpt in the error: under
  sudo/non-TTY dokku writes failure text to stdout, which the exec
  layer's stderr-only excerpt dropped — a bad image tag surfaced as a
  bare `exit 1` instead of "manifest unknown".

### Changed
- Reconcile convergence delegates to `backend.Apply` — the platform-
  backend contract's single converge primitive. The reconcile seam's
  parallel per-field setters are gone; the forks had already diverged
  (internal-mode ports shipped host mappings; Apply's checks-fingerprint
  archive deploys, resource-limit convergence, and `git:sync` build
  deploys never ran under reconcile). Storage stays reconciler-side
  (ADR-0009 host-dir lifecycle) via the explicit
  `ApplyOptions.SkipStorage` split. Secret env keys key on the shared
  `domain.SecretEnvKey` dash-kebab rule. Testing policy: ADR-0012
  amendment A1 bans fakes of TOP's own contracts — hermetic tiers run
  the real adapter over the scripted exec runner and the real SQLite
  store (existing per-package test backends scheduled for replacement).

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
