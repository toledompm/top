#!/bin/sh
# run-with-shim.sh — run any command with the tier-2 dokku shim on PATH
# (ADR-0012). Prepends test/shim/bin to PATH and exports the shim's
# environment, then cleans up its log/state files afterwards.
#
# Usage:
#   test/shim/run-with-shim.sh [--fixtures DIR] [--log FILE] [--keep] -- CMD [ARGS...]
#
#   --fixtures DIR   shim registry directory (default: test/fixtures/dokku,
#                    which is YAML source form — the shim only reads its
#                    registry layout, so point at a dir written by
#                    fakeexec.WriteShimFixtures for exact-argv matches)
#   --log FILE       where the shim appends its JSON argv log
#                    (default: a mktemp file; --keep preserves it)
#   --keep           do not delete the log/state afterwards; the log path is
#                    printed on stdout start so you can inspect it
#
# Example (inside the make test-shim flow the Go test does this itself):
#   test/shim/run-with-shim.sh -- dokku apps:list --format json

set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

fx="$root/test/fixtures/dokku"
log=""
keep=0

while [ "$#" -gt 0 ]; do
    case "$1" in
        --fixtures)
            [ "$#" -ge 2 ] || { echo "run-with-shim.sh: $1 needs a value" >&2; exit 2; }
            fx=$2; shift 2 ;;
        --log)
            [ "$#" -ge 2 ] || { echo "run-with-shim.sh: $1 needs a value" >&2; exit 2; }
            log=$2; shift 2 ;;
        --keep)
            keep=1; shift ;;
        --)
            shift; break ;;
        -*)
            echo "run-with-shim.sh: unknown option $1" >&2; exit 2 ;;
        *)
            break ;;
    esac
done

[ "$#" -gt 0 ] || { echo "run-with-shim.sh: no command given" >&2; exit 2; }

# Make sure the shim script itself is executable (fresh clones may lose +x).
[ -x "$here/bin/dokku" ] || chmod +x "$here/bin/dokku"

if [ -z "$log" ]; then
    log=$(mktemp "${TMPDIR:-/tmp}/top-shim-log.XXXXXX")
fi
state="${DOKKU_SHIM_STATE:-$log.state}"

trap '[ "$keep" -eq 1 ] || rm -rf "$state" "$log"' EXIT

echo "shim log: $log (fixtures: $fx)" >&2

PATH="$here/bin:$PATH" \
DOKKU_SHIM_FIXTURES="$fx" \
DOKKU_SHIM_LOG="$log" \
DOKKU_SHIM_STATE="$state" \
    "$@"
