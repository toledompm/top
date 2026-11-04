#!/bin/sh
# SPDX-License-Identifier: MIT
# install-top.sh — TOP's one-liner entrypoint. Thin and inspectable: it only
# ensures a `topctl` binary and forwards EVERYTHING to `topctl install` (which
# itself prefights, plans, and approval-gates; ADR-0005/0009). This script
# NEVER installs Go, never edits PATH, and makes no host changes of its own.
set -eu

# Release-artifact base (T-507). Artifacts per release $pin: raw
# `topctl-<os>-<arch>` binaries + ONE `checksums.txt` (sha256, released by
# .goreleaser.yaml). Semantics:
#   * default (TOPCTL_VERSION_PIN unset): pin defaults to v0.1.0 — same
#     behavior as before slotting the pin in (base URL below matches the old
#     literal); verification stays convenience-tier: per-artifact .sha256
#     when present, else warn + proceed.
#   * TOPCTL_VERSION_PIN=vX.Y.Z (any digest-worthy pin): verification is
#     REQUIRED — checksums.txt must exist and the artifact must match its
#     entry; missing/mismatched => refuse (exit 3). No per-artifact .sha256
#     falls through.
# Both values remain fictional example hosts; the owner sets the real base.
TOP_ARTIFACT_BASE_URL="${TOP_ARTIFACT_BASE_URL-https://releases.example.com/eng/top}"
_pin_user="${_pin_user-}"  # "1" iff the operator actually set TOPCTL_VERSION_PIN
case "${_pin_user+x}" in x) _pin_user=1 ;; esac
TOPCTL_VERSION_PIN="${TOPCTL_VERSION_PIN-v0.1.0}"
_release_url="$TOP_ARTIFACT_BASE_URL/$TOPCTL_VERSION_PIN"

usage() {
  cat >&2 <<EOF
usage: install-top.sh [--check] <git-repo-url> [topctl install flags...]

  --check   run \`topctl doctor\` only; change nothing
Runs \`topctl doctor\` read-only first (prints findings, continues), then
execs \`topctl install\` with all of your forwarded flags (e.g. --dry-run,
--install-dokku, --deploy-key, --known-hosts, --confirm). See:
./topctl install --help
EOF
  exit 2
}
case "${1-}" in
  -h|--help) usage ;;
esac

topctl="$(command -v topctl 2>/dev/null || true)"

# verify_sha256 <expected-hex> <file>: portable sha256 check (sha256sum on
# Linux; shasum -a 256 on macOS runners/owners). Succeeds silently or fails.
verify_sha256() {
  want="$1"; file="$2"
  if command -v sha256sum >/dev/null 2>&1; then
    echo "$want  $file" | sha256sum -c - >/dev/null 2>&1
  elif command -v shasum >/dev/null 2>&1; then
    echo "$want  $file" | shasum -a 256 -c - >/dev/null 2>&1
  else
    return 1
  fi
}
if [ -z "$topctl" ]; then
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"; case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  # (b) released artifact at $TOPCTL_VERSION_PIN (checksum semantics above)
  artifact="topctl-$os-$arch"
  if curl -fsSLo ./topctl "$_release_url/$artifact" 2>/dev/null; then
    chmod +x ./topctl
    verified=0
    # checksums.txt (the pin vocabulary) — required under an explicit pin,
    # optional convenience otherwise
    if curl -fsSLo ./checksums.txt "$_release_url/checksums.txt" 2>/dev/null; then
      want="$(grep "  $artifact\$" ./checksums.txt | awk 'NR==1{print $1}')"
      if [ -n "$want" ] && verify_sha256 "$want" ./topctl; then
        verified=1
      elif [ "$_pin_user" = 1 ]; then
        rm -f ./topctl ./checksums.txt
        echo "install-top: TOPCTL_VERSION_PIN=$TOPCTL_VERSION_PIN: artifact failed checksums.txt verification; refusing" >&2
        exit 3
      else
        echo "install-top: artifact not in checksums.txt (unexpected); proceeding unverified — set TOPCTL_VERSION_PIN to require verification" >&2
      fi
      rm -f ./checksums.txt
    elif [ "$_pin_user" = 1 ]; then
      rm -f ./topctl
      echo "install-top: TOPCTL_VERSION_PIN=$TOPCTL_VERSION_PIN: no checksums.txt published for that release; refusing unverifiable artifact" >&2
      exit 3
    fi
    # unpinned convenience: per-artifact .sha256 still honored when present
    if [ "$verified" = 0 ] && curl -fsSLo ./topctl.sha256 "$_release_url/$artifact.sha256" 2>/dev/null; then
      sum="$(awk '{print $1}' ./topctl.sha256)"
      verify_sha256 "$sum" ./topctl || { rm ./topctl ./topctl.sha256; echo "install-top: checksum mismatch; refusing the artifact" >&2; exit 3; }
    elif [ "$verified" = 0 ] && [ "$_pin_user" != 1 ]; then
      echo "install-top: no checksums.txt/.sha256 for this artifact; proceeding unverified (prefer TOPCTL_VERSION_PIN=vX.Y.Z)" >&2
    fi
    rm -f ./topctl.sha256
    topctl=./topctl
  # (c) build from source (needs go >= 1.24)
  elif command -v go >/dev/null 2>&1; then
    echo "install-top: building topctl from source (go $(go version | awk '{print $3}'); go 1.24+ required)" >&2
    go build -o ./topctl ./cmd/topctl 2>/dev/null || go build -o ./topctl github.com/toledompm/top/cmd/topctl
    topctl=./topctl
  fi
fi
[ -n "${topctl:-}" ] || {
  echo "install-top: cannot get topctl: not on PATH, no release reachable at $_release_url (TOPCTL_VERSION_PIN=$TOPCTL_VERSION_PIN), and no go toolchain (>=1.24) for the source path" >&2
  exit 3
}

# Read-only preflight every run (unless the operator only asked for it, or is
# explicitly hand-checking via --dry-run: still cheap + harmless — doctor
# mutates nothing, so it always runs; findings print, then install re-preflights).
case "${1-}" in
  --check)
    "$topctl" doctor ${2:+"$@"}; rc=$?
    echo "install-top: --check done (doctor exit $rc). Fix failures above, then re-run without --check." >&2
    exit "$rc"
    ;;
esac

[ $# -ge 1 ] || usage
echo "install-top: preflight (read-only) —" >&2
"$topctl" doctor || echo "install-top: preflight above shows failures/warnings; continuing — 'topctl install' runs its own preflight and will stop on failures." >&2
exec "$topctl" install "$@"
