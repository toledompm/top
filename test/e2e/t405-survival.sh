#!/bin/sh
# t405-survival.sh — T-405 tier-4 e2e: M4 acceptance 8 (TOP-down survival).
# SPDX-License-Identifier: MIT
#
#   1. `docker kill` the controller container: the TOP-managed workloads +
#      monitoring stack keep running (prometheus, grafana, node-exporter,
#      cadvisor + an optional sample user app); the controller revives via
#      its `always` restart policy (core/top-controller.yaml).
#   2. While the controller is down, a spec change is pushed — its
#      convergence must NOT happen while TOP is down; it reconciles on
#      return via the poll backstop (missed changes reconcile on return).
#   3. `topctl recover restart` (the controllerless recovery path,
#      ADR-0005) restarts TOP; `recover diagnose` is finding-free.
#   4. NO redeploy across the whole exercise: the core app's running
#      container keeps its image identity across the kill-revive and the
#      recover-restart — i.e. the git:from-image pipeline is never re-run
#      for core-top-controller (the bootstrap adoption is one-shot,
#      T-402/ADR-0005; controller self-updates are human-gated via
#      `topctl deploy core/top-controller`), and no adoption activity
#      re-runs on the record surface.
#
# Runs ON a disposable VM (tier 4, ADR-0012) with acceptance 2 already
# green. Reads lib.sh's env contract.
#
# Usage: sh test/e2e/t405-survival.sh <platform-repo git url>

set -u

. "$(dirname "$0")/lib.sh"

PLATFORM_REPO=${1:?usage: t405-survival.sh <platform-repo git url or checkout path>}
GIT_BRANCH=${TOP_E2E_GIT_BRANCH:-main}
PROBE_KEY=${TOP_E2E_PROBE_KEY:-E2E_PROBE_T405}
PROBE_FILE=${TOP_E2E_PROBE_FILE:-observability/grafana.yaml}

CONTROLLER_APP=${TOP_E2E_CONTROLLER_APP:-core-top-controller}
USER_APP=${TOP_E2E_USER_APP:-} # optional sample tenant app name
PROM_APP=${TOP_E2E_PROM_APP:-observability-prometheus}
GRAFANA_APP=${TOP_E2E_GRAFANA_APP:-observability-grafana}
NODE_APP=${TOP_E2E_NODE_APP:-observability-node-exporter}
CADV_APP=${TOP_E2E_CADV_APP:-observability-cadvisor}

for bin in jq git "$TOPCTL_BIN" "$DOKKU_BIN" "$DOCKER_BIN"; do
    command -v "$bin" >/dev/null 2>&1 || die "precondition: '$bin' not on PATH"
done
[ -f "$PLATFORM_REPO/$PROBE_FILE" ] || die "precondition: not a platform repo checkout: $PLATFORM_REPO"

# the controller's running process container + its restart policy (the
# kill-revival assertion is meaningless without `always`)
CTRL_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
[ -n "$CTRL_CTR" ] || die "precondition: controller container not running"
[ "$("$DOCKER_BIN" inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$CTRL_CTR")" = "always" ] \
    || die "controller is not on restart policy always"
for app in "$PROM_APP" "$GRAFANA_APP" "$NODE_APP" "$CADV_APP"; do
    docker_running_name "$app.web." >/dev/null || die "precondition: $app not running"
done

WT=$(mktemp -d "${TMPDIR:-/tmp}/top-e2e-t405.XXXXXX") || die "mktemp"
trap 'rm -rf "$WT"' EXIT INT HUP
git clone --branch "$GIT_BRANCH" "$PLATFORM_REPO" "$WT/repo" >/dev/null \
    || die "cannot clone platform repo: $PLATFORM_REPO"
COMMITTER="git -c user.name=top-e2e -c user.email=top-e2e@example.invalid"

# fingerprint <container> — the redeploy audit anchor. A deploy (the
# git:from-image pipeline + rollout) always lands a NEW container; a
# restart keeps the running container (and its bound image identity).
# Either way, the image the core app runs never moves without a
# human-gated self-deploy.
fingerprint() { "$DOCKER_BIN" inspect -f '{{.Image}} {{.Name}}' "$1"; }

FINGERPRINT_BEFORE=$(fingerprint "$CTRL_CTR")

# ---------------------------------------------------------------- scenario 1
## push a change that MUST NOT converge while TOP is down.

inject_env_probe "$WT/repo/$PROBE_FILE" "$PROBE_KEY" '"probe-survival-down"' \
    || die "cannot inject $PROBE_KEY into $PROBE_FILE (unexpected env-block layout?)"
cd "$WT/repo" || die "cd repo"
git add "$PROBE_FILE"
$COMMITTER commit -q -m "t405: down-probe (must NOT converge while controller is down)"
SHA_DOWN=$(git rev-parse HEAD)
pass "down-probe pushed (sha $SHA_DOWN)"

## `docker kill` the controller; observe the down window.

"$DOCKER_BIN" kill "$CTRL_CTR" >/dev/null || die "docker kill controller failed"
KILLED_AT=$(date +%s)
pass "controller container killed"

# survival evidence, three rounds across the down window: the monitoring
# stack (and the optional user app) keep RUNNING — healthcheck-level
# evidence, not just container presence.
for round in 1 2 3; do
    for app in "$PROM_APP" "$GRAFANA_APP" "$NODE_APP" "$CADV_APP"; do
        ctr=$(docker_running_name "$app.web.")
        [ -n "$ctr" ] || die "$app container vanished while controller down (round $round)"
        [ "$("$DOCKER_BIN" inspect -f '{{.State.Running}}' "$ctr")" = "true" ] \
            || die "$app not running while controller down (round $round)"
    done
    pass "monitoring stack survives round $round (TOP down)"
    if [ -n "$USER_APP" ]; then
        ctr=$(docker_running_name "$USER_APP.web.")
        [ -n "$ctr" ] || die "user app $USER_APP down while TOP is down"
        pass "user app $USER_APP survives round $round"
    fi
    sleep 5
done

## while-down assertion: the down-probe sha does NOT converge.

if history_has succeeded controller "$SHA_DOWN" || history_has succeeded webhook "$SHA_DOWN"; then
    die "down-probe CONVERGED while the controller was down (impossible by design)"
fi
pass "down-probe NOT converged while controller down"

## revival 1: `always` restart policy.

DEADLINE=$(($(date +%s) + 120))
until [ -n "$(docker_running_name "$CONTROLLER_APP.web.")" ]; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "controller never revived via restart policy (120s)"
    sleep 3
done
REVIVED_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
pass "controller container revived via restart policy"

# ---------------------------------------------------------------- scenario 2
## revival 2: `topctl recover restart` (the controllerless repair path).

"$TOPCTL_BIN" recover restart --confirm >/dev/null \
    || die "topctl recover restart failed"
pass "recover restart issued"

DEADLINE=$(($(date +%s) + 120))
until [ -n "$(docker_running_name "$CONTROLLER_APP.web.")" ]; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "controller never came back after recover restart (120s)"
    sleep 3
done
REVIVED_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
pass "controller back after recover restart"

# controller healthy: recover diagnose (the doctor vocabulary over the
# live controller) is a finding-free pass.
if ! "$TOPCTL_BIN" recover diagnose >/dev/null 2>&1; then
    die "topctl recover diagnose reported findings after revival"
fi
pass "recover diagnose clean (controller healthy)"

# reconcile resumes: the missed (down-probe) change converges now.
POLL_BUDGET=$((${TOP_E2E_POLL_SECONDS} * 3 + 90))
DEADLINE=$(($(date +%s) + POLL_BUDGET))
until history_has succeeded controller "$SHA_DOWN"; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "down-probe never reconciled after revival (${POLL_BUDGET}s)"
    sleep 5
done
pass "missed change reconciled on return (poll, sha $SHA_DOWN)"
assert_eq "revival applied the down-probe value" "probe-survival-down" \
    "$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY")"

# ---------------------------------------------------------------- scenario 3
## NO redeploy across the whole exercise.

FINGERPRINT_AFTER=$(fingerprint "$REVIVED_CTR")
[ "$FINGERPRINT_BEFORE" = "$FINGERPRINT_AFTER" ] \
    || die "controller image/container identity CHANGED across the exercise (a self-redeploy ran): before=[$FINGERPRINT_BEFORE] after=[$FINGERPRINT_AFTER]"
pass "git:from-image never re-ran for core-top-controller (fingerprint stable across kill + recovery restart)"

# adoption stays one-shot: no re-adoption signal appears on the record
# surface AMONG THE ROWS RECORDED DURING THIS EXERCISE (historical rows are
# out of scope; the T-402 ledger flip records once; restarts never re-adopt
# — internal/reconcile/adopt.go's adopt-aware posture).
SINCE=$(date -u -d "@$KILLED_AT" '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)
if topctl_json history | jq -e --arg since "$SINCE" \
    '.operations[] | select(.startedAt > $since and ((.error? // "") | test("bootstrap-owned")))' >/dev/null 2>&1; then
    die "a bootstrap-owned signal appeared in exercise-window history (adoption re-ran?)"
fi
pass "no re-adoption on the record surface (exercise window)"

echo
echo "t405: green (acceptance 8: workloads+monitoring survive TOP-down; recover restart revives; no redeploy)"
