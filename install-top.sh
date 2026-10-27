#!/bin/sh
# SPDX-License-Identifier: MIT
# install-top.sh — TOP's one-liner entrypoint. Thin and inspectable: it only
# ensures a `topctl` binary and forwards EVERYTHING to `topctl install` (which
# itself prefights, plans, and approval-gates; ADR-0005/0009). This script
# NEVER installs Go, never edits PATH, and makes no host changes of its own.
set -eu

# Pinned release-artifact base. SHA256 contract: checksums live in each
# release's notes (the accompanying .sha256 file is verified when present —
# convenience, not cryptographic trust from the environment). Pinned the same
# way docs/contracts/installer.md pins everything else; example value host.
TOP_ARTIFACT_BASE_URL="https://releases.example.com/eng/top/v0.1.0"

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
if [ -z "$topctl" ]; then
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"; case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  # (b) released artifact, version-pinned base URL above (checksum contract)
  if curl -fsSLo ./topctl "$TOP_ARTIFACT_BASE_URL/topctl-$os-$arch" 2>/dev/null; then
    chmod +x ./topctl
    if curl -fsSLo ./topctl.sha256 "$TOP_ARTIFACT_BASE_URL/topctl-$os-$arch.sha256" 2>/dev/null; then
      sum="$(awk '{print $1}' ./topctl.sha256)"
      echo "$sum  ./topctl" | sha256sum -c - >/dev/null 2>&1 || { rm ./topctl ./topctl.sha256; echo "install-top: checksum mismatch; refusing the artifact" >&2; exit 3; }
      rm ./topctl.sha256
    else
      echo "install-top: no .sha256 published for this artifact; proceeding unverified (prefer a pinned release)" >&2
    fi
    topctl=./topctl
  # (c) build from source (needs go >= 1.24)
  elif command -v go >/dev/null 2>&1; then
    echo "install-top: building topctl from source (go $(go version | awk '{print $3}'); go 1.24+ required)" >&2
    go build -o ./topctl ./cmd/topctl 2>/dev/null || go build -o ./topctl github.com/toledompm/top/cmd/topctl
    topctl=./topctl
  fi
fi
[ -n "${topctl:-}" ] || {
  echo "install-top: cannot get topctl: not on PATH, no pinned release reachable at TOP_ARTIFACT_BASE_URL, and no go toolchain (>=1.24) for the source path" >&2
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
