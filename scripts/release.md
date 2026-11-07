# Owner release procedure (T-507)

> This file documents how the OWNER cuts a real TOP release. Nothing here is
> automated by CI on purpose: publishing needs a token, and TOP's CI is
> fork-safe with no secrets (ADR-0012/0014, ci.yml header).

## Pinned tooling

goreleaser is invoked by TAG via `go run`, never added to `go.mod`:

- **Pin: `github.com/goreleaser/goreleaser/v2@v2.19.0`** — the latest stable
  v2 line as of this task (v2.20.x exists only as `*-nightly` tags; verified
  against goreleaser's tag list 2026-11-03).
- goreleaser v2.19.0 **requires go >= 1.27.3**: `go run` auto-switches
  toolchains — do NOT set `GOTOOLCHAIN=local` for release targets (other
  repo targets keep their local preference). CI provides go 1.24 via
  setup-go and auto-switches the same way.
- Re-pin by editing BOTH places: `RELEASE_TOOL` in the `Makefile` and the
  header of `.goreleaser.yaml` (plus this file).

## What a release contains

Per `.goreleaser.yaml`: raw binaries `topctl-linux-amd64` /
`topctl-linux-arm64` plus ONE `checksums.txt` (sha256). No containers/images:
TOP steers service images through Dokku (`git:from-image`, `git:sync`), not a
registry. `main.version` is ldflags-injected from the tag, which is the same
string `topctl version` reports and `top_build_info{version}` carries.

## Owner flow, step by step

```sh
# 0. Preconditions: on main, tests green, CHANGELOG.md [Unreleased] renamed
#    to the new version heading (keep a changelog-style heading + date).
make test && make vet && make hygiene

# 1. Cut the tag (tags are vX.Y.Z; goreleaser refuses non-semver tags).
tag=v0.2.0
git tag -a "$tag" -m "top $tag" && git push origin main "$tag"

# 2. CI runs the release-dry job on the tag (no secrets, publishes nothing):
#    goreleaser check + full snapshot build + smoke test of the artifact.
#    Watch https://github.com/toledompm/top/actions (release-dry workflow).
#    The release job (.github/workflows/release.yml) then PUBLISHES the
#    GitHub Release with topctl-linux-{amd64,arm64} + checksums.txt
#    attached automatically — with the repository's own GITHUB_TOKEN, no
#    owner-local goreleaser run required.
#
# 3. Owner follow-up (only if the workflow needs manual intervention):
export GITHUB_TOKEN="$(security find-generic-password -s top-goreleaser -w)"  # or your own secret store
make release           # goreleaser release --clean; refuses without the token

# 4. Post-publish, mirror checksums.txt next to the artifacts and make the
#    release URL under TOP_ARTIFACT_BASE_URL serve: topctl-linux-{amd64,arm64}
#    + checksums.txt. install-top.sh verifies against checksums.txt when
#    TOPCTL_VERSION_PIN is set.

# 5. Sanity: ./topctl version -o json must show data.topctl == $tag
#    (same string feeds top_build_info via internal/obs).
```

## Local preview (no secrets, any machine)

- `make release-dry-run` — full matrix snapshot from clean state.
- `make release-check` — config validation; needs an `origin` remote, so it
  skips gracefully without one (CI's release-dry job is the real gate).

## Known gaps / handoff to T-508

- `dist/` layout: physical binaries live under
  `dist/topctl_linux_<arch>*/topctl`; the published name is
  `topctl-linux-<arch>` as recorded in `checksums.txt` (verified locally).
- Snapshot runs are not byte-reproducible (`main.version` embeds the run
  date); tagged releases are the reference artifacts.
- goreleaser `check` also wants a git `origin` remote (scm introspection).
- No signing (cosign/gpg): not scoped; consider for a later hardening slice.
