#!/bin/sh
# t404-webhook-poll.sh — T-404 tier-4 e2e: M4 acceptance 3+4.
# SPDX-License-Identifier: MIT
#
#   scenario 1: a push to the seeded platform repo → provider webhook →
#   controller converges the service (evidence: the repo revision appears
#   as a SUCCEEDED op record with actor=webhook, correlated with OUR
#   X-GitHub-Delivery id; the probe env is live on the app; `topctl diff`
#   is clean; the pre-convergence drift surfaced a file:line spec origin).
#
#   scenario 2: the missed-webhook path — a second push is delivered
#   through nothing (and a deliberately FORGED signature is rejected 401,
#   the deterministic stand-in for a dropped delivery), the poll backstop
#   picks it up within its configured interval and converges with NO
#   webhook evidence on the op record (poll semantics: actor=controller).
#
# Runs ON a disposable VM (tier 4, ADR-0012) with acceptance 2 already
# green (`core` + `observability` reconciled). Reads lib.sh's env contract.
#
# Usage (repo checkout on the VM):
#   TOP_E2E_WEBHOOK_URL=https://top.example.com/webhooks/github \
#   sh test/e2e/t404-webhook-poll.sh <platform-repo git url>
#
# The argument $1 is the OPERATOR's platform repo (a Git URL or a path to
# a local checkout); the scripts never press into the operator's working
# copy — they clone a throwaway mirror. Tier-3-extended twin:
# internal/backend/dokku/e2etest covers the HMAC/dedup handler math.
#
# Scenario 2 precondition: the second push must reach the controller ONLY
# via poll — either run this script while the platform repo's real provider
# webhook is unconfigured/removed, or accept that a configured provider's
# own delivery would fail the "no webhook evidence" assertion BY DESIGN.

set -u

. "$(dirname "$0")/lib.sh"

PLATFORM_REPO=${1:?usage: t404-webhook-poll.sh <platform-repo git url or checkout path>}
GIT_BRANCH=${TOP_E2E_GIT_BRANCH:-main}
WEBHOOK_SECRET_FILE=${TOP_E2E_WEBHOOK_SECRET_FILE:-/var/lib/top/secrets/webhook-hmac}
WEBHOOK_URL=${TOP_E2E_WEBHOOK_URL:?usage: set TOP_E2E_WEBHOOK_URL to the controller webhook endpoint (fictional shape: https://top.example.com/webhooks/github)}

GRAFANA_APP=${TOP_E2E_GRAFANA_APP:-observability-grafana}
PROBE_KEY=${TOP_E2E_PROBE_KEY:-E2E_PROBE_T404}
PROBE_FILE=${TOP_E2E_PROBE_FILE:-observability/grafana.yaml}

for bin in jq openssl curl git "$TOPCTL_BIN" "$DOKKU_BIN" "$DOCKER_BIN"; do
    command -v "$bin" >/dev/null 2>&1 || die "precondition: '$bin' not on PATH"
done
[ -r "$WEBHOOK_SECRET_FILE" ] || die "precondition: webhook secret not readable: $WEBHOOK_SECRET_FILE"
case "$PLATFORM_REPO" in
    git://*|https?://*|ssh://*|*git@*) ;; # remote URL: the clone itself proves the repo shape
    *) [ -f "$PLATFORM_REPO/observability/grafana.yaml" ] || die "precondition: not a platform repo checkout: $PLATFORM_REPO" ;;
esac

WEBHOOK_SECRET=$(cat "$WEBHOOK_SECRET_FILE")

WT=$(mktemp -d "${TMPDIR:-/tmp}/top-t404.XXXXXX") || die "mktemp"
cd "$WT" || die "cd WT"
trap 'rm -rf "$WT"' EXIT INT HUP

git clone --branch "$GIT_BRANCH" "$PLATFORM_REPO" repo >/dev/null \
    || die "cannot clone platform repo: $PLATFORM_REPO (branch $GIT_BRANCH)"
cd repo || die "cd repo"

commit_probe() { # commit_probe <quoted-value> <message> — probes the one
    # env item this scenario watches; the push and its caught sha are echoed.
    inject_env_probe "$PROBE_FILE" "$PROBE_KEY" "$1" \
        || die "cannot inject $PROBE_KEY into $PROBE_FILE (unexpected env-block layout?)"
    git add "$PROBE_FILE"
    git -c user.name=top-e2e -c user.email=top-e2e@example.invalid \
        commit -q --allow-empty -m "$2" || die "git commit failed"
    # The controller resolves the REPO (its mirror), not this checkout:
    # without the push the committed sha is unresolvable and the
    # convergence assertions would race against a sha that never landed
    # (tier-4 verified 2026-11-03). The operator protocol carries the
    # push auth as usual.
    git push -q origin "$GIT_BRANCH" || die "git push failed (no credentials for $PLATFORM_REPO?)"
    git rev-parse HEAD
}

deliver_webhook() { # deliver_webhook <sha> <delivery-id> [sig-hex]
    # prints the HTTP status; signs with OUR secret unless a sig is given.
    printf '{"ref":"refs/heads/%s","after":"%s"}' "$GIT_BRANCH" "$1" > "$WT/body.json"
    sig=${3:-$(hmac_sha256_hex "$WEBHOOK_SECRET" "$WT/body.json")}
    webhook_send "$WEBHOOK_URL" "$WEBHOOK_SECRET" "$WT/body.json" "$2" "$sig"
}

# ---------------------------------------------------------------- scenario 1
## 1a. mutate the spec + push

SHA1=$(commit_probe '"probe-webhook-1"' "t404: webhook-path probe")

pass "push 1 landed (sha $SHA1)"

## 1b. drift evidence BEFORE convergence: TOP surfaces the change with the
## spec's file:line origin (the canonical source surface).

DIFFJSON=$(topctl_json diff)
echo "$DIFFJSON" | jq -e \
    --arg app "$GRAFANA_APP" \
    'def nsname($a): ($a | split("-") | (.[0] + "/" + (.[1:] | join("-")))); [.data.services[] | select(.service | nsname($app)) | .source | test("observability/grafana.yaml:[0-9]+$")] | length > 0' \
    >/dev/null \
    || die "diff does not surface $GRAFANA_APP with a file:line origin: $DIFFJSON"
pass "drift row for $GRAFANA_APP carries file:line spec origin"

## 1c. deliver the provider webhook (GitHub HMAC convention)

CODE=$(deliver_webhook "$SHA1" "t404-webhook-1-$SHA1")
assert_eq "signed webhook delivery accepted (202)" "202" "$CODE"

## 1d. convergence evidence: a SUCCEEDED op row, actor=webhook,
## revision=our sha, correlated with our Delivery-ID.

DEADLINE=$(($(date +%s) + 150))
until history_has succeeded webhook "$SHA1" "t404-webhook-1-$SHA1"; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "webhook run for $SHA1 never converged (150s)"
    sleep 5
done
pass "op record: actor=webhook revision=$SHA1 correlated=t404-webhook-1-$SHA1"

## 1e. deployed evidence: the probe env is live on the Dokku app

DEADLINE=$(($(date +%s) + 60))
until [ "$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY")" = "probe-webhook-1" ]; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "grafana app never carried probe-webhook-1 (60s)"
    sleep 3
done
pass "app carries the new value (new revision deployed)"

## 1f. post-convergence diff is clean (exit 0)

DEADLINE=$(($(date +%s) + 180))
until "$TOPCTL_BIN" diff >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "topctl diff never went clean (180s)"
    sleep 5
done
pass "topctl diff clean — scenario 1 (acceptance 3) complete"

# ---------------------------------------------------------------- scenario 2
## 2a. second push; the webhook is dropped deliberately. Proof of the
## deliberate drop: the SAME payload under a forged signature gets the
## deterministic 401 (rejected before any enqueue), and after convergence
## no webhook-actor row may exist for this sha.

SHA2=$(commit_probe '"probe-poll-2"' "t404: missed-poll probe (webhook dropped)")
pass "push 2 landed (sha $SHA2)"

CODE=$(deliver_webhook "$SHA2" "t404-forged-$SHA2" \
    "$(hmac_sha256_hex top-e2e-deliberately-wrong-secret "$WT/body.json")")
assert_eq "tampered webhook delivery rejected (401 — the deliberate drop)" "401" "$CODE"

## 2b. the poll backstop converges it within the configured interval.

POLL_BUDGET=$((${TOP_E2E_POLL_SECONDS} * 3 + 90))
DEADLINE=$(($(date +%s) + POLL_BUDGET))
until history_has succeeded controller "$SHA2"; do
    [ "$(date +%s)" -ge "$DEADLINE" ] && die "poll never picked up $SHA2 within ${POLL_BUDGET}s"
    sleep 5
done
pass "poll run recorded: actor=controller revision=$SHA2"

## 2c. the poll convergence carries NO webhook evidence.

if topctl_json history | jq -e --arg rev "$SHA2" \
    '.data.operations[] | select(.actor == "webhook" and .revision == $rev)' >/dev/null; then
    die "webhook evidence exists for $SHA2 — poll assertion violated"
fi
pass "sha2 converged with NO webhook evidence on the record"

## 2d. value evidence: the poll-deployed revision carries probe-2.

POLLVAL=$("$DOKKU_BIN" config:get "$GRAFANA_APP" "$PROBE_KEY")
assert_eq "poll path deployed the new value" "probe-poll-2" "$POLLVAL"

echo
echo "t404: green (acceptance 3 webhook → converged + acceptance 4 missed → poll)"
