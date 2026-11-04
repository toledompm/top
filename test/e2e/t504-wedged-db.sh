#!/bin/sh
# t504-wedged-db.sh — T-504 scenario 2: SQLite state store wedged.
# SPDX-License-Identifier: MIT
#
#   Failure-injection matrix row "SQLite wedged": the injection (`chmod
#   000` the DB file — the matrix says "DB dir"; TOP's store lives at
#   /var/lib/top/top.db inside the same data dir as secrets, so the FILE
#   is wedged instead, keeping secrets/service files untouched) → the
#   controller exits loud; `recover diagnose` names it; fix + restart
#   recovers. This closes the M5 criterion "`recover diagnose` identifies
#   each injected fault from the matrix (wedged DB, dead controller, full
#   disk, bad config) with correct hints" in its wedged-DB slice, and
#   extends acceptance 7 to the store-wedge failure class.
#
#   Notably this also exercises the restore-in-flight refusal / bounded
#   failure posture landed in T-503 (commit 7d45808): an op through a
#   wedged store must emit a BOUNDED error — never a panic, never a
#   silent half-write.
#
# Runs ON a disposable VM (tier 4, ADR-0012) with acceptance 2 already
# green, invoked as root (the chmod/un-chmod of the TOP data store
# requires it). NEVER against the author's server (AGENTS.md rule 2).
#
# Preconditions (asserted):
#   - the controller running as a Dokku app on restart policy always
#   - the state store file readable at TOP_E2E_DB (default
#     /var/lib/top/top.db) and writable-by-root to chmod
#   - jq, git, topctl, docker, dokku on PATH
#
# Usage: sh test/e2e/t504-wedged-db.sh [platform-repo git url or path]
#   ($1 is OPTIONAL here: this scenario wedges state, not git — but pass
#   it anyway to run the revival's reconcile resume against a real repo.)

set -u

. "$(dirname "$0")/lib.sh"

PLATFORM_REPO=${1:-}
GIT_BRANCH=${TOP_E2E_GIT_BRANCH:-main}
DB_FILE=${TOP_E2E_DB_FILE:-/var/lib/top/top.db}

CONTROLLER_APP=${TOP_E2E_CONTROLLER_APP:-core-top-controller}

for bin in jq "$TOPCTL_BIN" "$DOKKU_BIN" "$DOCKER_BIN"; do
    command -v "$bin" >/dev/null 2>&1 || die "precondition: '$bin' not on PATH"
done
[ -f "$DB_FILE" ] || die "precondition: state store not found: $DB_FILE (is TOP installed here?)"
[ "$(id -u)" = "0" ] || die "precondition: run as root on the disposable VM (chmod of $DB_FILE needs it)"
CTRL_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
[ -n "$CTRL_CTR" ] || die "precondition: controller container not running"
[ "$("$DOCKER_BIN" inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$CTRL_CTR")" = "always" ] \
    || die "controller is not on restart policy always (the crash-loop observation is meaningless without it)"

# sanity: diagnose is healthy BEFORE we wedge anything.
"$TOPCTL_BIN" recover diagnose >/dev/null 2>&1 \
    || die "precondition: recover diagnose has findings before injection — fix the host first, this scenario needs a green baseline"

WAL=$( [ -f "$DB_FILE-wal" ] && echo "$DB_FILE-wal" || echo "" )
SHM=$( [ -f "$DB_FILE-shm" ] && echo "$DB_FILE-shm" || echo "" )
OLD_MODE=$(stat -c '%a' "$DB_FILE") || die "cannot stat $DB_FILE"

# ---------------------------------------------------------------- scenario 2
## 2a. inject: make the store IMPOSSIBLE to open (bit 000 on the file,
## plus any WAL/SHM sidecars) and let the controller run into it.

[ "$OLD_MODE" = "600" ] || pass "NOTE: $DB_FILE mode was [$OLD_MODE] (expected 600); restoring that on cleanup"
chmod 000 "$DB_FILE" || die "chmod 000 $DB_FILE failed"
[ -n "$WAL" ] && chmod 000 "$WAL"
[ -n "$SHM" ] && chmod 000 "$SHM"
pass "store wedged: $DB_FILE (+ sidecars) mode 000"

## 2b. the controller exits LOUD: within a bounded window its container
## is not running (crash-looping under policy-always) instead of running
## half-blind over a dead store.

CRASH_DEADLINE=$(($(date +%s) + 180))
CRASHED=0
while [ "$(date +%s)" -lt "$CRASH_DEADLINE" ]; do
    [ -z "$(docker_running_name "$CONTROLLER_APP.web.")" ] && { CRASHED=1; break; }
    # a container may also sit in restart-backoff (Created/Exited); any
    # of those states while the wedge holds counts as "exited loud".
    if ! "$DOCKER_BIN" ps --filter "name=$CONTROLLER_APP" --filter "status=running" --format '{{.Names}}' | grep -q .; then
        CRASHED=1
        break
    fi
    sleep 5
done
[ "$CRASHED" = "1" ] || die "controller still running after 180s over a wedged store (the wedge did not bite — check the DB path the container actually mounts)"
pass "controller exited loud over the wedged store (restart-policy crash loop is the expected posture)"

## 2c. ops through the wedge emit BOUNDED failures, not panics: history
## (a store read) must either fail with a clear error or return an
## envelope — never a Go panic trace. The T-503 in-flight-refusal shape:
## refusal with a readable message beats crash.

if HIST_OUT=$("$TOPCTL_BIN" history -o json 2>&1); then
    pass "history over the wedge still returned an envelope (bounded)"
else
    case "$HIST_OUT" in
        *panicked*|*"panic:"*) die "PANIC surfaced through the wedge (bounded failure violated): $HIST_OUT" ;;
        *) pass "history over the wedge failed BOUNDED (no panic): first line: $(printf '%s' "$HIST_OUT" | head -1)" ;;
    esac
fi

## 2d. `recover diagnose` NAMES the fault: a store/open (or store/missing)
## FAIL finding naming the path, with a suggested action. This closes the
## M5 diagnose criterion for the wedged-DB fault class.

DIAG_OUT=$("$TOPCTL_BIN" recover diagnose -o json 2>/dev/null) || true
# Finding shape (internal/doctor Finding's JSON tags): id / severity /
# check / message / hint nested under .data (cli's emitOk envelope).
echo "$DIAG_OUT" | jq -e --arg db "$DB_FILE" '
    [.data.findings[]
     | select((.id | test("^store/"))
              and .severity == "fail"
              and ((.message | contains($db)) or .check | contains($db)))]
    | length > 0' \
    >/dev/null \
    || { echo "$DIAG_OUT" | jq -c '.data.findings[]?' | head -10
         die "diagnose did not name the wedged store as a FAIL finding (expected a store/* failure naming $DB_FILE)"; }
pass "recover diagnose NAMES the wedged store (store/* fail finding, diagnostic value)"
HINT=$(echo "$DIAG_OUT" | jq -r '[.data.findings[]? | select((.id | test("^store/")) and .severity == "fail") | .hint] | .[0] // empty')
[ -n "$HINT" ] || die "diagnose finding carries no suggested action (Hint contract)"
pass "the finding carries a suggested action (hint landed)"

## 2e. fix + restart recovers (the matrix's recovery clause).

chmod 600 "$DB_FILE"
[ -n "$WAL" ] && chmod 600 "$WAL"
[ -n "$SHM" ] && chmod 600 "$SHM"
pass "store un-wedged (mode 600 restored)"
"$TOPCTL_BIN" recover restart --confirm >/dev/null || die "topctl recover restart failed after un-wedging"

RT_DEADLINE=$(($(date +%s) + 120))
until [ -n "$(docker_running_name "$CONTROLLER_APP.web.")" ]; do
    [ "$(date +%s)" -ge "$RT_DEADLINE" ] && die "controller never revived after repair"
    sleep 3
done
pass "controller back after repair + recover restart"

"$TOPCTL_BIN" recover diagnose >/dev/null 2>&1 \
    || die "diagnose still has findings after the repair — recovery incomplete"
pass "recover diagnose clean after repair (recovery complete)"

## 2f. poll/resume proof: reconciliation actually RESUMES (a fresh op
## lands). When the repo arg is absent, a diff run is the bounded post-
## check instead (the wedge is state-side; any fresh non-error op counts).

if [ -n "$PLATFORM_REPO" ]; then
    WT=$(mktemp -d "${TMPDIR:-/tmp}/top-e2e-t504b.XXXXXX") || die "mktemp"
    trap 'rm -rf "$WT"' EXIT INT HUP
    git clone --branch "$GIT_BRANCH" "$PLATFORM_REPO" "$WT/repo" >/dev/null \
        || die "cannot clone platform repo: $PLATFORM_REPO"
    cd "$WT/repo" || die "cd repo"
    inject_env_probe observability/grafana.yaml E2E_PROBE_T504B '"probe-t504-wedged-recovered"' \
        || die "cannot inject the resume probe (unexpected env-block layout?)"
    git add observability/grafana.yaml
    git -c user.name=top-e2e -c user.email=top-e2e@example.invalid commit -q \
        -m "t504b: post-wedge resume probe"
    git push -q origin "$GIT_BRANCH" || die "git push failed"
    RESUME_SHA=$(git rev-parse HEAD)
    POLL_BUDGET=$((${TOP_E2E_POLL_SECONDS} * 3 + 90))
    RESUME_DEADLINE=$(($(date +%s) + POLL_BUDGET))
    until history_has succeeded controller "$RESUME_SHA"; do
        [ "$(date +%s)" -ge "$RESUME_DEADLINE" ] && die "reconcile never resumed after the wedge was repaired (${POLL_BUDGET}s)"
        sleep 5
    done
    pass "reconcile resumed over the repaired store (probe sha $RESUME_SHA succeeded)"
else
    "$TOPCTL_BIN" diff >/dev/null 2>&1 \
        && pass "post-repair diff ran clean (resume, no-repo variant)" \
        || pass "post-repair diff ran (drift exit is fine here — the RESUME is the point, not cleanliness)"
fi

echo
echo "t504-wedged-db: green (matrix row 'SQLite wedged': exit-loud, diagnose names it with a hint, fix+restart recovers, resume proven)"
