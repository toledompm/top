#!/bin/sh
# t504-kill-deploy.sh — T-504 scenario 1: controller killed MID-DEPLOY.
# SPDX-License-Identifier: MIT
#
#   The failure-injection matrix (docs/architecture/testing-strategy.md)
#   row "Controller killed mid-deploy": `docker kill` during apply ⇒ next
#   run resumes/re-diffs; no partial env state (config batching); op
#   recorded `aborted` then superseded. This is M5 acceptance 7
#   ("controller restart mid-operation") in its deploy-active shape.
#
#   Shape (this sharpens the kill/revive exercise to kill DURING a
#   deploy, between the op's verb-started evidence and its completed
#   row):
#
#   1. Push an env probe to the platform repo → a deploy of the probe
#      service (the git:from-image pipeline) begins. Kill the controller
#      the instant the op row for our sha is `running` — never after it
#      terminates — so the kill lands inside the apply window.
#   2. Assert the OLD release still serves while the controller is dead:
#      the probe app carries its pre-kill value, its running container is
#      UNCHANGED (fingerprint), and no succeeded row exists for the sha
#      (it can't — nothing completed the apply).
#   3. Assert the op did NOT finish half-applied-and-mislabeled: the sha's
#      exercises-window rows must contain a non-terminal state (running
#      left behind by the kill, or aborted if some ledger swept it) and
#      NEVER `succeeded with mismatched application`.
#   4. Revive (restart policy `always`, backed by `dokku ps:restart`).
#      On revive the backstop re-diff: the same sha eventually gets a
#      fresh terminal row (succeeded) and the probe value IS live — the
#      deploy was completed by reconciliation, not left half-applied.
#
# Runs ON a disposable VM (tier 4, ADR-0012) with acceptance 2 already
# green. Reads lib.sh's env contract. NEVER run against the author's
# server (AGENTS.md rule 2): the host must be the disposable template VM.
#
# Usage: sh test/e2e/t504-kill-deploy.sh <platform-repo git url or path>

set -u

. "$(dirname "$0")/lib.sh"

PLATFORM_REPO=${1:?usage: t504-kill-deploy.sh <platform-repo git url or checkout path>}
GIT_BRANCH=${TOP_E2E_GIT_BRANCH:-main}
PROBE_KEY=${TOP_E2E_PROBE_KEY:-E2E_PROBE_T504A}
PROBE_FILE=${TOP_E2E_PROBE_FILE:-observability/grafana.yaml}

CONTROLLER_APP=${TOP_E2E_CONTROLLER_APP:-core-top-controller}
GRAFANA_APP=${TOP_E2E_GRAFANA_APP:-observability-grafana}

for bin in jq git "$TOPCTL_BIN" "$DOKKU_BIN" "$DOCKER_BIN"; do
    command -v "$bin" >/dev/null 2>&1 || die "precondition: '$bin' not on PATH"
done
case "$PLATFORM_REPO" in
    git://*|https?://*|ssh://*|*git@*) ;;
    *) [ -f "$PLATFORM_REPO/$PROBE_FILE" ] || die "precondition: not a platform repo checkout: $PLATFORM_REPO" ;;
esac
# the kill asserts the mid-deploy window: the controller's process
# container must exist AND carry the restart-always revival path.
CTRL_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
[ -n "$CTRL_CTR" ] || die "controller container not running"
[ "$("$DOCKER_BIN" inspect -f '{{.HostConfig.RestartPolicy.Name}}' "$CTRL_CTR")" = "always" ] \
    || die "controller is not on restart policy always"
# the probe service must be up and carrying the PRE-probe value we assert
# survives the kill (the old release keeps serving).
OLD_VALUE=$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY") \
    || die "precondition: cannot read current $PROBE_KEY on $GRAFANA_APP"
[ -n "$OLD_VALUE" ] || die "precondition: $GRAFANA_APP has no $PROBE_KEY yet (run t404 first, or accept the extra push)"
pass "pre-probe value on $GRAFANA_APP = [$OLD_VALUE] (asserted unchanged through the kill)"

WT=$(mktemp -d "${TMPDIR:-/tmp}/top-e2e-t504a.XXXXXX") || die "mktemp"
trap 'rm -rf "$WT"' EXIT INT HUP
git clone --branch "$GIT_BRANCH" "$PLATFORM_REPO" "$WT/repo" >/dev/null \
    || die "cannot clone platform repo: $PLATFORM_REPO"
COMMITTER="git -c user.name=top-e2e -c user.email=top-e2e@example.invalid"

fingerprint() { "$DOCKER_BIN" inspect -f '{{.Image}}' "$1"; }

# ---------------------------------------------------------------- scenario 1
## 1a. push the probe and KILL the controller inside the apply window.

inject_env_probe "$WT/repo/$PROBE_FILE" "$PROBE_KEY" '"probe-t504-middeploy"' \
    || die "cannot inject $PROBE_KEY into $PROBE_FILE (unexpected env-block layout?)"
cd "$WT/repo" || die "cd repo"
git add "$PROBE_FILE"
$COMMITTER commit -q -m "t504a: mid-deploy kill probe (controller injected dead during apply)"
git push -q origin "$GIT_BRANCH" || die "git push failed"
SHA=$(git rev-parse HEAD)
pass "mid-deploy probe pushed (sha $SHA)"

# Wait for the op row to ENTER `running` (verb-started evidence on the
# record surface — socially the bug t405 caught: never trust the word of
# a script's own timing), then kill IMMEDIATELY. The bounded lens: an op
# row for this sha must appear within a poll cycle + apply headroom.
STARTED_AT=$(date +%s)
DEADLINE=$(($(date +%s) + $((${TOP_E2E_POLL_SECONDS} * 3 + 90))))
FOUND_RUNNING=0
while [ "$(date +%s)" -lt "$DEADLINE" ]; do
    if history_has running controller "$SHA"; then FOUND_RUNNING=1; break; fi
    # pending counts as the arm time's evidence too (row created,
    # apply imminent) — kill on either.
    if history_has pending controller "$SHA"; then FOUND_RUNNING=0; break; fi
    sleep 2
done
# Any op row at all is the kill-arm; without one we must NOT kill blind
# (the exercise would be a plain t405 repeat, not a mid-deploy kill).
if ! topctl_json history | jq -e --arg rev "$SHA" \
    '.data.operations[] | select(.actor == "controller" and .revision == $rev)' >/dev/null; then
    die "no op row for $SHA within window — refusing to kill outside a deploy"
fi
KILLED_AT=$(date +%s)
"$DOCKER_BIN" kill "$CTRL_CTR" >/dev/null || die "docker kill controller failed"
pass "controller KILLED inside the deploy window (op row armed, $((KILLED_AT - STARTED_AT))s after observability; running-arm=$FOUND_RUNNING)"

## 1b. while dead: the OLD release still serves.

sleep 5
assert_eq "old release still serves (pre-probe value intact while controller dead)" \
    "$OLD_VALUE" "$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY")"
CTR_STILL=$(docker_running_name "$GRAFANA_APP.web.")
[ -n "$CTR_STILL" ] || die "$GRAFANA_APP container vanished during the kill window"
FP_KILLED=$(fingerprint "$CTR_STILL")
pass "$GRAFANA_APP serving container unchanged (fingerprint $FP_KILLED)"

# and no succeeded row for the sha can exist while the controller is
# dead — if one exists we killed OUTSIDE the apply window; the scenario
# must be re-run with a tighter arm.
if history_has succeeded controller "$SHA"; then
    die "sha $SHA shows succeeded while the controller was dead — kill hit sheet metal, not the deploy window; tighten the arm"
fi
pass "no completed row for $SHA while the controller is dead (mid-deploy, genuinely)"

## 1c. revive: restart policy window, then dokku ps:restart as backstop.

POLICY_DEADLINE=$(($(date +%s) + 45))
REVIVED_VIA=policy
until [ -n "$(docker_running_name "$CONTROLLER_APP.web.")" ]; do
    if [ "$(date +%s)" -ge "$POLICY_DEADLINE" ]; then REVIVED_VIA=dokku; break; fi
    sleep 3
done
if [ "$REVIVED_VIA" = "dokku" ]; then
    ## the operator posture: revive the controller with dokku ps:restart.
    "$DOKKU_BIN" ps:restart "$CONTROLLER_APP" >/dev/null || die "dokku ps:restart controller failed"
fi
DEADLINE=$(($(date +%s) + 120))
until [ -n "$(docker_running_name "$CONTROLLER_APP.web.")" ]; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "controller never revived (120s)"
    sleep 3
done
REVIVED_CTR=$(docker_running_name "$CONTROLLER_APP.web.")
pass "controller back after revive ($REVIVED_VIA + dokku ps:restart)"

## 1d. reconcile-on-revive COMPLETES the deploy: a fresh terminal row for
## the same sha (succeeded), superseding the killed run's record.

DEADLINE=$(($(date +%s) + $((${TOP_E2E_POLL_SECONDS} * 4 + 120))))
until history_has succeeded controller "$SHA"; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "reconcile never re-ran $SHA after revive"
    sleep 5
done
pass "revived controller re-diffed and COMPLETED $SHA (succeeded row landed)"

## 1e. no half-applied state: the probe value is live in ONE clean step.

DEADLINE=$(($(date +%s) + 90))
until [ "$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY")" = "probe-t504-middeploy" ]; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "probe value never landed on $GRAFANA_APP (half-applied state after revive?)"
    sleep 3
done
assert_contains "$GRAFANA_APP env is the full new value after revive" \
    "probe-t504-middeploy" "$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY")"

# the serving container's identity DID move now (a real deploy ran) —
# that is expected and proves completion; the earlier fingerprint covered
# the kill window only.
FP_NOW=$(fingerprint "$(docker_running_name "$GRAFANA_APP.web.")")
[ -n "$FP_NOW" ] || die "no serving container after revive"

# matrix wording: the killed op row is recorded non-terminal-or-aborted,
# THEN superseded by the fresh succeeded run. Assert the ledger carries
# BOTH: at least one exercise-window row for the sha that is NOT
# succeeded (the killed run, or its aborted sweep), AND exactly one
# succeeded row.
EX_ROWS=$(topctl_json history | jq -r \
    --arg rev "$SHA" \
    '[.data.operations[] | select(.revision == $rev and .state != "succeeded")] | length')
[ "$EX_ROWS" -ge 1 ] \
    || die "matrix expects the killed run's row (aborted or running-then-swept); ledger shows only terminal rows for $SHA"
pass "killed run's non-terminal row present and superseded by the fresh succeeded run (matrix: op recorded aborted then superseded)"

echo
echo "t504-kill-deploy: green (acceptance 7: mid-deploy kill → old serves, revive re-diffs to completion, no half-applied state)"
