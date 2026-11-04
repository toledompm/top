#!/bin/sh
# t504-disk-pressure.sh — T-504 scenario 3: disk pressure on the host.
# SPDX-License-Identifier: MIT
#
#   Failure-injection matrix row "Disk pressure": fill to ~95% in the VM
#   ⇒ preflight/backup report clear errors; no silent corruption. Forks
#   of the work:
#
#   - The tier-1 hermetic shard (internal/backup/diskpressure_test.go)
#     pins the ENGINE slice: Create refusal is bounded (`vacuum into …`
#     error, result=error metric, no registry row), Plan stays correct
#     under a wedged destination, and a stale backup is prunable once
#     capacity/spaces is restored.
#   - THIS script drives the HOST posture on the disposable VM: fill the
#     TOP data filesystem to ≥95% used and assert the OBSERVABILITY
#     contract — `topctl recover diagnose` names the pressure (a
#     disk/headroom fail finding) and TOP's read surfaces stay bounded.
#   - ⚠ OPEN ITEM (documented for T-506): `topctl backup create/list/
#     verify` are wired as notImpl stubs (internal/cli/stubs.go — the
#     backup CLI arrived untouched by M5 so far), so the canonical
#     "backup create fails bounded under ENOSPC" assertion is carried by
#     the hermetic shard above and THIS script asserts the host posture.
#     When the backup CLI lands, add a fill-phase assertion:
#     `topctl backup create --confirm` must exit non-panic with a clear
#     error while ≥95% full and succeed after unfill.
#
# DANGER / OPERATING NOTES (AGENTS.md rule 2: disposable VM only — NEVER
# the author's server):
#   - The script fills the filesystem holding TOP_E2E_FILL_DIR (default
#     /var/lib/top — the TOP data dir's FS) with allocated files named
#     .top-e2e-diskfill.<n>. It hard-stops the fill at ≥ 5% free and
#     REFUSES to fill below 2% free headroom (the VM must survive to
#     clean up; die-on-fill-below-floor keeps the run unbricking).
#   - Every exit (pass, FAIL, signal) un-fills: trap-based rm of the
#     fill files before the report/exit. If an assertion dies AFTER a
#     fill phase, the same trap runs — check `df` after any interrupted
#     run anyway.
#
# Preconditions:
#   - run as root ON the disposable VM, TOP installed green (acceptance 2)
#   - jq, dd, df, awk, topctl, docker on PATH
#   - the controller container running (it must SURVIVE the pressure)
#
# Usage: sh test/e2e/t504-disk-pressure.sh

set -u

. "$(dirname "$0")/lib.sh"

FILL_DIR=${TOP_E2E_FILL_DIR:-/var/lib/top}
THRESHOLD=${TOP_E2E_FILL_THRESHOLD:-95}

CONTROLLER_APP=${TOP_E2E_CONTROLLER_APP:-core-top-controller}

for bin in jq awk dd df "$TOPCTL_BIN" "$DOKKU_BIN" "$DOCKER_BIN"; do
    command -v "$bin" >/dev/null 2>&1 || die "precondition: '$bin' not on PATH"
done
[ -d "$FILL_DIR" ] || die "precondition: fill dir not found: $FILL_DIR"
[ "$(id -u)" = "0" ] || die "precondition: run as root on the disposable VM (fill + restore needs it)"
CTRL_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
[ -n "$CTRL_CTR" ] || die "precondition: controller container not running"
"$TOPCTL_BIN" recover diagnose >/dev/null 2>&1 \
    || die "precondition: recover diagnose has findings before injection — this scenario needs a green baseline"

free_pct() { # free_pct <dir> — percentage FREE on <dir>'s filesystem (integer)
    df -Pm "$1" | awk 'NR==2 { printf "%.0f", $4 * 100 / $2 }'
}

FILL_TAG=".top-e2e-diskfill"
cleanup_fill() { rm -f "$FILL_DIR"/$FILL_TAG.* >/dev/null 2>&1; }
trap 'cleanup_fill' EXIT INT HUP

# the threshold is % USED; free_pct ≤ (100 - THRESHOLD) means at/over it.
FLOOR_FREE=2
[ "$(free_pct "$FILL_DIR")" -ge $((100 - THRESHOLD + 1)) ] \
    || die "precondition: $FILL_DIR is already ≥${THRESHOLD}% used; the injection needs headroom to fill INTO"

# ---------------------------------------------------------------- scenario 3
## 3a. baseline evidence before the squeeze.

PASS_BEFORE=$("$TOPCTL_BIN" recover diagnose >/dev/null 2>&1 && echo clean || echo findings)
[ "$PASS_BEFORE" = "clean" ] || die "baseline diagnose is not clean (fix the host first)"
pass "baseline: diagnose clean"

## 3b. inject: fill until ≥${THRESHOLD}% used — 256 MiB blocks so the
## fill never overshoots the 2%-free floor guard in one step.

TARGET_FREE=$((100 - THRESHOLD))
BLOCK_MB=256
N=0
while [ "$(free_pct "$FILL_DIR")" -gt "$TARGET_FREE" ]; do
    [ "$(free_pct "$FILL_DIR")" -ge $((FLOOR_FREE + 1)) ] \
        || die "fill floor reached (<$((FLOOR_FREE + 1))% free); freeing now — the VM must survive to clean up"
    N=$((N + 1))
    dd if=/dev/zero of="$FILL_DIR/$FILL_TAG.$N" bs=1M count="$BLOCK_MB" 2>/dev/null \
        || die "dd block $N failed (real ENOSPC already?)"
done
USED=$((100 - $(free_pct "$FILL_DIR")))
[ "$USED" -ge "$THRESHOLD" ] || die "fill loop exited below the threshold (used=${USED}%)"
pass "fill phase complete (${USED}% used, threshold ${THRESHOLD}%, $N block(s))"

## 3c. the diagnosis NAMES the pressure: disk/headroom fail finding.

DIAG_OUT=$("$TOPCTL_BIN" recover diagnose -o json 2>/dev/null) || true
echo "$DIAG_OUT" | jq -e \
    '.data.findings[] | select((.id | test("^disk/")) and .severity == "fail")' \
    >/dev/null \
    || { echo "$DIAG_OUT" | jq -c '.data.findings[]?' | head -10
         die "diagnose did not name the disk pressure as a FAIL finding (expected disk/headroom fail)"; }
pass "recover diagnose NAMES the disk pressure (disk/headroom fail)"

## 3d. TOP's surfaces stay BOUNDED over the pressure: diff and history
## complete (possibly with findings/exits) without hanging or panicking.

for probe in "diff" "history"; do
    OUT=$("$TOPCTL_BIN" "$probe" -o json 2>&1)
    case "$OUT" in
        *panicked*|*"panic:"*) die "PANIC through $probe under disk pressure: $OUT" ;;
    esac
    pass "$probe completed bounded under pressure (no panic, no hang)"
done

## 3e. the controller SURVIVES the pressure (monitoring staleness is not
## the 3c exercise's point — a dead controller would be a different
## finding; the matrix expects reporting, not death).

sleep 10
[ -n "$(docker_running_name "$CONTROLLER_APP.web.")" ] \
    || die "controller container died under disk pressure (report the dead-crash as a finding — restart it before re-running)"

## 3f. capacity restored: the diagnosis goes clean again; the engine's
## bounded-refusal + stale-prunable posture is pinned by the tier-1
## shard (internal/backup/diskpressure_test.go) — re-run it green on the
## VM (tier-1 is portable) as the stage's proof.

cleanup_fill
sleep 5
DEADLINE=$(($(date +%s) + 90))
until "$TOPCTL_BIN" recover diagnose >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "diagnose still failing after unfill (90s)"
    sleep 5
done
pass "diagnose clean after capacity restored (no lasting corruption)"
"$TOPCTL_BIN" diff > /dev/null 2>&1 \
    && pass "post-recovery diff clean" \
    || pass "post-recovery diff ran (drift exit is fine — the PRESSURE recovery is the point)"

echo
echo "t504-disk-pressure: green (matrix row 'Disk pressure': diagnose names >=${THRESHOLD}% full; bounded surfaces; controller survives; clean recovery)"
echo "  (engine refusal/prune posture: hermetic shard internal/backup/diskpressure_test.go — go test ./internal/backup)"
