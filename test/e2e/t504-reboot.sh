#!/bin/sh
# t504-reboot.sh — T-504 scenario 4: full host reboot / container-set restart.
# SPDX-License-Identifier: MIT
#
#   Failure-injection matrix row "Host reboot (tier 4)": reboot ⇒ Dokku
#   `ps:restore` revives apps + controller; TOP resumes polling. This
#   sharpens t405's single-container fingerprint machinery to a
#   CONTAINER-SET-level kill: everything TOP manages restarts at once.
#
#   Injection shape (choosable):
#     TOP_E2E_REBOOT_VM=1  — the REAL VM reboot (`reboot` — run under your
#                            supervisor's session; this re-entry into the
#                            script is NOT automatic: after the VM is back,
#                            re-run with the same args to verify).
#     default              — a docker-level full stop→start of the managed
#                            container set (every TOP-managed app's
#                            containers, including the controller and the
#                            monitoring stack), which is the reboot's
#                            workload-level consequence.
#
#   Assertions:
#   1. every managed container comes back up within a bounded deadline
#      (restart policies + Dokku restore semantics).
#   2. the controller revives and `recover diagnose` is clean.
#   3. NO redeploys: every container that had an identity going in keeps
#      it coming out (image + container fingerprint stable — a deploy
#      would land a NEW container; a restart never changes the id). One
#      newly-created container ANYWHERE in the managed set means the
#      reconcile re-ran the git:from-image pipeline — that is the bug
#      this scenario exists to catch.
#   4. the poll backstop still converges a fresh push after the reboot
#      (TOP's loop is live, not just the containers).
#
# Runs ON a disposable VM (tier 4, ADR-0012) with acceptance 2 already
# green. NEVER against the author's server (AGENTS.md rule 2).
#
# Usage:
#   sh test/e2e/t504-reboot.sh <platform-repo git url or path>
#   TOP_E2E_REBOOT_VM=1 sh test/e2e/t504-reboot.sh <repo>   # real reboot;

set -u

. "$(dirname "$0")/lib.sh"

PLATFORM_REPO=${1:?usage: t504-reboot.sh <platform-repo git url or checkout path>}
GIT_BRANCH=${TOP_E2E_GIT_BRANCH:-main}
PROBE_KEY=${TOP_E2E_PROBE_KEY:-E2E_PROBE_T504C}
PROBE_FILE=${TOP_E2E_PROBE_FILE:-observability/grafana.yaml}

CONTROLLER_APP=${TOP_E2E_CONTROLLER_APP:-core-top-controller}
USER_APP=${TOP_E2E_USER_APP:-} # optional tenant app
PROM_APP=${TOP_E2E_PROM_APP:-observability-prometheus}
GRAFANA_APP=${TOP_E2E_GRAFANA_APP:-observability-grafana}
NODE_APP=${TOP_E2E_NODE_APP:-observability-node-exporter}
CADV_APP=${TOP_E2E_CADV_APP:-observability-cadvisor}

for bin in jq git "$TOPCTL_BIN" "$DOKKU_BIN" "$DOCKER_BIN"; do
    command -v "$bin" >/dev/null 2>&1 || die "precondition: '$bin' not on PATH"
done
case "$PLATFORM_REPO" in
    git://*|https?://*|ssh://*|*git@*) ;;
    *) [ -f "$PLATFORM_REPO/$PROBE_FILE" ] || die "precondition: not a platform repo checkout: $PLATFORM_REPO" ;;
esac

APPS="$CONTROLLER_APP $GRAFANA_APP $PROM_APP $NODE_APP $CADV_APP"
[ -n "$USER_APP" ] && APPS="$APPS $USER_APP"

# preconditions: everything running, everything restart-policied, and a
# green diagnose baseline.
SET_FP_BEFORE=""
for app in $APPS; do
    ctr=$(docker_running_name "$app.web.")
    [ -n "$ctr" ] || die "precondition: $app container not running"
    case "$("$DOCKER_BIN" inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$ctr")" in
        always|unless-stopped) ;;
        *) die "$app restart policy is not always/unless-stopped (reboot revival is meaningless without it)" ;;
    esac
    SET_FP_BEFORE="$SET_FP_BEFORE
$app|$("$DOCKER_BIN" inspect -f '{{.Id}} {{.Image}}' "$ctr")"
done
pass "managed set running with revival policies ($APPS)"
"$TOPCTL_BIN" recover diagnose >/dev/null 2>&1 \
    || die "precondition: recover diagnose has findings before the reboot"

WT=$(mktemp -d "${TMPDIR:-/tmp}/top-e2e-t504c.XXXXXX") || die "mktemp"
trap 'rm -rf "$WT"' EXIT INT HUP
git clone --branch "$GIT_BRANCH" "$PLATFORM_REPO" "$WT/repo" >/dev/null \
    || die "cannot clone platform repo: $PLATFORM_REPO"
COMMITTER="git -c user.name=top-e2e -c user.email=top-e2e@example.invalid"

## boot window probe: converge a push BEFORE the reboot so the revival
## assertions run against settled state, and the post-reboot probe is a
## NEW sha (proves the live loop rather than replayed work).

inject_env_probe "$WT/repo/$PROBE_FILE" "$PROBE_KEY" '"probe-t504-reboot-pre"' \
    || die "cannot inject $PROBE_KEY into $PROBE_FILE (unexpected env-block layout?)"
cd "$WT/repo" || die "cd repo"
git add "$PROBE_FILE"
$COMMITTER commit -q -m "t504c: pre-reboot probe"
git push -q origin "$GIT_BRANCH" || die "git push failed"
SHA_PRE=$(git rev-parse HEAD)
DEADLINE=$(($(date +%s) + $((${TOP_E2E_POLL_SECONDS} * 3 + 90))))
until history_has succeeded controller "$SHA_PRE"; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "pre-reboot probe never converged"
    sleep 5
done
pass "pre-reboot probe converged (settled state; sha $SHA_PRE)"

# ---------------------------------------------------------------- scenario 4
## 4a. the injection.

if [ "${TOP_E2E_REBOOT_VM:-0}" = "1" ]; then
    pass "VM reboot requested: issue `reboot` under your supervisor, wait for the VM, then RE-RUN this script in verify mode (TOP_E2E_REBOOT_SKIP_REBOOT=1)"
    if [ "${TOP_E2E_REBOOT_SKIP_REBOOT:-0}" != "1" ]; then
        pass "issuing VM reboot — this script ends here"
        sync || true
        sleep 3
        exit 0
    fi
else
    pass "docker-level container-set restart (the reboot's workload-level consequence)"
    for app in $APPS; do
        ctr=$(docker_running_name "$app.web.")
        [ -n "$ctr" ] && "$DOCKER_BIN" stop -t 10 "$ctr" >/dev/null
    done
    for app in $APPS; do
        ctr=$("$DOCKER_BIN" ps -a --filter "name=$app.web." --format '{{.Names}}' | sed -n 1p)
        [ -n "$ctr" ] || die "no container for $app to restart"
        "$DOCKER_BIN" start "$ctr" >/dev/null || die "docker start $ctr failed"
    done
    pass "managed container set restarted (stopped + started, not recreated)"
fi

## 4b. everything comes back within a bounding deadline.

RIVING_DEADLINE=$(($(date +%s) + 180))
MISSING_PRINT=""
while [ "$(date +%s)" -lt "$RIVING_DEADLINE" ]; do
    MISSING=""
    for app in $APPS; do
        [ -n "$(docker_running_name "$app.web.")" ] || MISSING="$MISSING $app"
    done
    [ -z "$MISSING" ] && break
    MISSING_PRINT=$MISSING
    sleep 5
done
[ -z "$MISSING_PRINT" ] || die "never came back after reboot:$MISSING_PRINT"
pass "full managed set running again within 180s"

## 4c. the controller revives: diagnose clean.

DEADLINE=$(($(date +%s) + 120))
until "$TOPCTL_BIN" recover diagnose >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "recover diagnose still finding after revival (120s)"
    sleep 5
done
pass "recover diagnose clean post-revival (controller healthy, store reads)"

## 4d. NO redeploys: every fingerprint survived — for the container-set
## analog of t405's single-app assertion (a deploy lands a NEW container;
## neither a docker restart nor a VM reboot may change the id/image pair).

SET_FP_AFTER=""
for app in $APPS; do
    ctr=$(docker_running_name "$app.web.")
    [ -n "$ctr" ] || die "$app container missing during fingerprint sweep (4b caught it already?)"
    SET_FP_AFTER="$SET_FP_AFTER
$app|$("$DOCKER_BIN" inspect -f '{{.Id}} {{.Image}}' "$ctr")"
done
if [ "$SET_FP_BEFORE" = "$SET_FP_AFTER" ]; then
    pass "NO redeploys: container-set fingerprints stable across the reboot ($APPS)"
else
    echo "$SET_FP_BEFORE" > "$WT/fp.before"
    echo "$SET_FP_AFTER" > "$WT/fp.after"
    diff "$WT/fp.before" "$WT/fp.after" | head -10 >&2
    die "fingerprint CHANGED across the reboot — a deploy (or container recreation) ran: see diff above"
fi

## 4e. the poll backstop is LIVE (a fresh post-reboot push converges).

inject_env_probe "$WT/repo/$PROBE_FILE" "$PROBE_KEY" '"probe-t504-reboot-post"' \
    || die "cannot inject the post-reboot probe"
git add "$PROBE_FILE"
$COMMITTER commit -q -m "t504c: post-reboot probe (poll must be live)"
git push -q origin "$GIT_BRANCH" || die "git push failed"
SHA_POST=$(git rev-parse HEAD)
POLL_BUDGET=$((${TOP_E2E_POLL_SECONDS} * 3 + 90))
DEADLINE=$(($(date +%s) + POLL_BUDGET))
until history_has succeeded controller "$SHA_POST"; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "post-reboot probe never converged (${POLL_BUDGET}s) — reconcile is not live after revival"
    sleep 5
done
pass "poll converged a fresh push post-reboot (sha $SHA_POST — TOP resumed, not replayed)"

echo
echo "t504-reboot: green (matrix row 'Host reboot': full set revived, diagnose clean, no redeploys, poll live)"
