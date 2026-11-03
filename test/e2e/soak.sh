#!/bin/sh
# soak.sh — the disposable tier-4 soak driver (fleet-run helper, M4/M3
# acceptance soak; fleet-verified 2026-11-03). Runs INSIDE the VM.
# SPDX-License-Identifier: MIT
#
# 60-minute loop: every SOAK_STEP seconds (--default 300) one trigger:
#   - a spec push (fresh probe key), delivered via the SIGNED WEBHOOK on
#     SOAK_WEBHOOK_EVERY-step cadence and SKIPPED otherwise (the poll
#     backstop repairs = the missed-webhook episode, M4 acceptance 4);
#   - docker-stats RSS samples for the controller container;
#   - a reason-log of op records (actor=webhook/controller, state).
# No soak asserts here: the caller greps the transcript at the end.
set -u

. /root/test-e2e/lib.sh

PLATFORM_REPO=${1:?soak.sh <platform-repo git url>}
STEPS=${SOAK_STEPS:?SOAK_STEPS (number of trigger cycles)}
STEP_SECONDS=${SOAK_STEP_SECONDS:-300}
GIT_BRANCH=${TOP_E2E_GIT_BRANCH:-main}
WEBHOOK_SECRET_FILE=${TOP_E2E_WEBHOOK_SECRET_FILE:-/var/lib/top/secrets/webhook-hmac}
WEBHOOK_URL=${TOP_E2E_WEBHOOK_URL:?set TOP_E2E_WEBHOOK_URL}
PROM_APP=${TOP_E2E_PROM_APP:-observability-prometheus}
SECRET=$(cat "$WEBHOOK_SECRET_FILE")

WT=/tmp/soak-repo
rm -rf "$WT" && git clone -q --branch "$GIT_BRANCH" "$PLATFORM_REPO" "$WT" || die "clone soak repo"
cd "$WT" || die "cd soak repo"
git config user.email top-e2e@example.invalid; git config user.name top-e2e
echo "agent-discipline: soak loop started at $(date -u +%FT%TZ), $STEPS steps x ${STEP_SECONDS}s"

n=1
while [ "$n" -le "$STEPS" ]; do
    KEY="E2E_SOAK_PROBE_$n"
    VALUE="soak-$n"
    . /root/test-e2e/lib.sh # fresh shell stdlib per step (assert+inject helpers)
    inject_env_probe "observability/grafana.yaml" "$KEY" "\"$VALUE\"" \
        || die "soak probe $KEY inject failed"
    git add -A >/dev/null
    git commit -q --allow-empty -m "soak step $n: probe $KEY"
    git push -q origin "$GIT_BRANCH" || die "soak push $n failed"
    SHA=$(git rev-parse HEAD)
    T0=$(date +%s)

    if [ $((n % 2)) -eq 1 ]; then
        BODY=$(mktemp); printf '{"ref":"refs/heads/%s","after":"%s"}' "$GIT_BRANCH" "$SHA" > "$BODY"
        SIG=$(hmac_sha256_hex "$SECRET" "$BODY")
        CODE=$(webhook_send "$WEBHOOK_URL" "$SECRET" "$BODY" "t-soak-$n-$SHA" "$SIG")
        rm -f "$BODY"
        echo "soak step $n: sha $SHA webhook=$CODE at $(date -u +%FT%TZ)"
    else
        echo "soak step $n: sha $SHA (webhook deliberately missed — poll backstop) at $(date -u +%FT%TZ)"
    fi

    # episode window: the convergence + one RSS sample + op-row snapshot.
    sleep 75
    RSS=$(docker stats --no-stream --format '{{.MemUsage}}' core-top-controller.web.1 2>/dev/null || echo "container-down")
    ROWS=$(topctl history -o json 2>/dev/null | jq -r --arg sha "$SHA" '[.data.operations[] | select(.revision == $sha)] | length')
    STATE=$(topctl history -o json 2>/dev/null | jq -r --arg sha "$SHA" '[.data.operations[] | select(.revision == $sha) | .state] | join(",")')
    echo "soak step $n evidence: rows=$ROWS state=$STATE rss=$RSS tsm=$(( $(date +%s) - T0 ))s"

    # sleep the REST of the step cadence.
    REM=$(( STEP_SECONDS - 75 ))
    [ "$REM" -gt 0 ] && sleep "$REM"
    n=$((n + 1))
done
echo "agent-discipline: soak loop ended at $(date -u +%FT%TZ)"
