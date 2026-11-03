# lib.sh — shared helpers for TOP's tier-4 (VM) e2e scripts (ADR-0012).
# SPDX-License-Identifier: MIT
#
# POSIX sh. Runnable on the disposable VM's bin/sh (dash/ash/bash all
# support the constructs used here; `local` is SHELVED as dash/ash/bash-
# supported non-POSIX-optional and kept — portability to POSIX-strict
# Solaris sh is not a target per ADR-0012).
#
# Host preconditions (documented here, asserted by the scripts themselves;
# every value below is FICTIONAL per ADR-0014 — the operator's env wins):
#
#   - a disposable VM with Docker + Dokku (tier-4 template shape,
#     docs/architecture/testing-strategy.md), TOP installed
#     (`topctl install <repo> --confirm` green, acceptance 2 already
#     reconciled), and `topctl run controller` alive as a Dokku app.
#   - jq, openssl, git on PATH.
#   - a reachable Git platform repo (branch = TOP_E2E_GIT_BRANCH, default
#     "main") that the controller polls, and the operator's webhook secret
#     file (/var/lib/top/secrets/webhook-hmac, is the SecretProvider's
#     webhook-hmac default from internal/webhook/README.md).
#
# Environment contract (all optional, fictional defaults):
#   TOPCTL_BIN  (default "topctl")   DOKKU_BIN (default "dokku")
#   DOCKER_BIN  (default "docker")
#   TOP_E2E_POLL_SECONDS  — the controller's configured poll interval
#                           (default 60 = internal/config's default)
#
# Scripts run ON the VM (not over ssh): tier 4 is self-contained, no host
# identity is ever embedded in any tracked file.

set -u

TOPCTL_BIN=${TOPCTL_BIN:-topctl}
DOKKU_BIN=${DOKKU_BIN:-dokku}
DOCKER_BIN=${DOCKER_BIN:-docker}
TOP_E2E_POLL_SECONDS=${TOP_E2E_POLL_SECONDS:-60}

# --- output / assertion helpers ------------------------------------------------

pass() { printf 'PASS %s\n' "$1"; }

# die <message> — prints a loud failure line and exits the script.
die() { printf 'FAIL %s\n' "$1" >&2; exit 1; }

assert_eq() {
    if [ "$2" = "$3" ]; then pass "$1"; return 0; fi
    die "$1: got [$2] want [$3]"
}

assert_contains() {
    case "$2" in
        *"$3"*) pass "$1"; return 0 ;;
    esac
    die "$1: [$2] does not contain [$3]"
}

# assert_not_contains <desc> <haystack> <needle> — negated (used for the
# "no webhook evidence" poll assertions).
assert_not_contains() {
    case "$2" in
        *"$3"*) die "$1: [$2] must NOT contain [$3]" ;;
    esac
    pass "$1"
}

# wait_until <deadline_epoch> <description> <probe-cmd ...> — probes a
# predicate on a 5-second rhythm until it exits with 0 or the deadline
# passes. The predicate's silence on failure keeps the output sane; the
# caller's deadline carries the budget.
wait_until() {
    _wu_deadline=$1; _wu_desc=$2; shift 2
    while [ "$(date +%s)" -lt "$_wu_deadline" ]; do
        if "$@" >/dev/null 2>&1; then pass "$_wu_desc"; return 0; fi
        sleep 5
    done
    die "$_wu_desc: timed out"
}

sleep_for() { # sleep_for <seconds> — named for readability in scripts
    sleep "$1"
}

# --- webhook helpers (T-404) ----------------------------------------------------
# The scripts simulate the Git provider's delivery: GitHub-convention HMAC
# (internal/webhook's fixed semantics — X-Hub-Signature-256 + X-GitHub-
# Event: push + X-GitHub-Delivery). Nothing of the payload is ever desired
# state; the handler only parses ref for audit logging (ADR-0003), so a
# minimal {"ref": ...} body is a faithful signed delivery.

# hmac_sha256_hex <secret> <file-with-body> — prints hex(HMAC-SHA256(secret,
# exact file bytes)) — the same computation Go's crypto/hmac does in
# webhook.verify (tested byte-for-byte in lib_test.go).
hmac_sha256_hex() {
    openssl dgst -sha256 -hmac "$1" -binary < "$2" | od -An -tx1 | tr -d ' \n'
}

# webhook_send <url> <secret> <body-file> <delivery-id> <signature-hex>
# prints the HTTP status code (000 = transport failure). curl IS a VM
# precondition (add it to the tier-4 template if missing).
webhook_send() {
    curl -s -o /dev/null -w '%{http_code}' \
        -X POST "$1" \
        -H 'Content-Type: application/json' \
        -H "X-Hub-Signature-256: sha256=$5" \
        -H 'X-GitHub-Event: push' \
        -H "X-GitHub-Delivery: $4" \
        --data-binary "@$3"
}

# topctl_json <args...> — topctl with one-line JSON output (its documented
# envelope shape; the scripts never parse human text).
topctl_json() {
    "$TOPCTL_BIN" "$@" -o json 2>/dev/null
}

# history_has <state> <actor> <revision> [correlation-id] — predicate:
# some operation row in topctl history matches the columns (empty
# correlation-id matches any).
history_has() {
    _hh_state=$1 _hh_actor=$2 _hh_rev=$3 _hh_corr=${4:-}
    topctl_json history | jq -e \
        --arg state "$_hh_state" --arg actor "$_hh_actor" \
        --arg rev "$_hh_rev" --arg corr "$_hh_corr" \
        '.operations[] | select(.state == $state and .actor == $actor and .revision == $rev) | if $corr == "" then . else select(.correlationId == $corr) end' \
        >/dev/null
}

# history_rows_after <epoch> — count operation rows started after an epoch
# (startedAt is RFC3339 UTC; jq compares strings safely because both are
# zero-padded fixed-width).
history_rows_after() {
    topctl_json history \
        | jq -r --arg since "$(date -u -d "@$1" +%Y-%m-%dT%H:%M:%S 2>/dev/null || date -u -j -f '%s' "$1" +%Y-%m-%dT%H:%M:%S)" \
              '[.operations[] | select(.startedAt > $since)] | length'
}

# --- platform-repo probe editing (lib_test.go exercises these) -----------------
# Managed env probes: an env item line inserted under the spec's `env:`
# mapping (strict spec: keys match [A-Za-z_][A-Za-z0-9_]*, plain strings
# only). The probe key/value is deliberately fictional.

# inject_env_probe <file> <key> <quoted-value> — idempotent insert after
# the exact two-space-indented `  env:` header line.
inject_env_probe() {
    if grep -q "^[ 	]*$2:" "$1"; then set_env_probe "$1" "$2" "$3"; return 0; fi
    # entry indent matches the env block's first existing sibling (else 2sp)
    INDENT=$(grep -A1 "^[ 	]*env:[ 	]*\$" "$1" | sed -n 2p | sed -n "s/^\([ 	]*\).*/\1/p")
    case "$INDENT" in '') INDENT="  " ;; esac
    awk -v line="${INDENT}$2: $3" '{
        print
        if ($0 ~ /^[ 	]*env:[ 	]*$/) { print line; done = 1 }
    }
    END { if (!done) exit 3 }' "$1" > "$1.tmp" || { rm -f "$1.tmp"; return 3; }
    mv "$1.tmp" "$1"
}

# set_env_probe <file> <key> <quoted-value> — rewrites an existing probe
# line's value; aborts if the key has multiple or binding-unclear lines.
set_env_probe() {
    if [ "$(grep -c "^[ 	]*$2[ 	]*:" "$1")" -ne 1 ]; then return 3; fi
    sed "s/^\([ 	]*\)$2[ 	]*:.*/\1$2: $3/" "$1" > "$1.tmp" || { rm -f "$1.tmp"; return 3; }
    mv "$1.tmp" "$1"
}

# docker_running_name <pattern> — prints the first running container name
# matching a name filter, empty when none.
docker_running_name() {
    "$DOCKER_BIN" ps --filter "status=running" --filter "name=$1" --format '{{.Names}}' | sed -n 1p
}
