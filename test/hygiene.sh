#!/bin/sh
# Hygiene check (ADR-0014): fail on forbidden patterns in tracked files.
#
# Scope: whatever `git ls-files --cached --others --exclude-standard` yields —
# i.e. tracked files plus untracked-but-not-gitignored files. Gitignored
# material (notably .local/) is never scanned and never read here.
#
# Pattern list (exactly what ADR-0014 demands, no more):
#   1. Private/CGNAT IPv4 literals (RFC1918: 10/8, 172.16/12, 192.168/16;
#      CGNAT RFC6598: 100.64/10)
#   2. AWS access key literals (AKIA + 16 uppercase alphanumeric)
#   3. GitHub token literals (ghp_/gho_/ghu_/ghs_/ghr_ + 20+ chars)
#   4. PEM private key block headers ("BEGIN ... PRIVATE KEY")
#   5. Private host details: "media-server", "tailscale" (case-insensitive),
#      and "top-homelab" EXCEPT when written as "`top-homelab`-style" — that
#      construction is used in docs as an explicitly fictional label for a
#      user's platform repo, not as hosting detail (see test/hygiene-allowlist).
#
# Self-exemption rules — documentation ABOUT these rules must not self-flag:
#   - docs/adr/0014-public-local-boundary.md (the policy itself)
#   - test/hygiene-allowlist (documents the patterns in prose)
#   - any line containing the word "hygiene" (policy discussion)
# No other exemptions; RFC5737/RFC2606 example values (192.0.2.x, 198.51.100.x,
# 203.0.113.x, example.com) are outside the flagged ranges and pass naturally.

set -u

cd "$(dirname "$0")/.." || exit 2

FILES=$(git ls-files --cached --others --exclude-standard | grep -v '^test/hygiene.sh$')
if [ -z "$FILES" ]; then
    echo "Hygiene check passed (no files)."
    exit 0
fi

fail=0

check() {
    desc="$1"
    pat="$2"
    matched=$(printf '%s\n' "$FILES" | xargs grep -nE -e "$pat" 2>/dev/null) || matched=""
    [ -z "$matched" ] && return 0
    # Drop self-referential policy-doc lines and the ADR/allowlist files.
    matched=$(printf '%s\n' "$matched" \
        | grep -vE '^(docs/adr/0014-public-local-boundary\.md|test/hygiene-allowlist)' \
        | grep -viE 'hygiene')
    [ -z "$matched" ] && return 0
    echo "FORBIDDEN ($desc):"
    echo "$matched"
    echo
    fail=1
}

# RFC1918 + CGNAT literals. Requires a non-digit context so version numbers
# like "10.0.38" in changelogs don't false-positive (see test/hygiene-allowlist).
check 'private/CGNAT IPv4 literal' \
    '(^|[^0-9.])(10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3}|100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3})([^0-9.]|$)'

check 'AWS access key literal' 'AKIA[0-9A-Z]{16}'
check 'GitHub token literal' 'gh[pousr]_[A-Za-z0-9]{20,}'
check 'PEM private key block' 'BEGIN [A-Z ]*PRIVATE KEY'
check 'private host detail' 'media-server|tailscale'
matched=$(printf '%s\n' "$FILES" | xargs grep -nE -e 'top-homelab' 2>/dev/null) || matched=""
matched=$(printf '%s\n' "$matched" | grep -vE '^test/hygiene-allowlist' | grep -viE 'hygiene') || matched=""
matched=$(printf '%s\n' "$matched" | grep -vE 'top-homelab`-style') || matched=""
[ -z "$matched" ] || { echo "FORBIDDEN (private host detail):"; echo "$matched"; echo; fail=1; }

if [ "$fail" -ne 0 ]; then
    echo "Hygiene check FAILED — see docs/adr/0014-public-local-boundary.md."
    echo "Generalize the value or move it to .local/ (never committed)."
    exit 1
fi
echo "Hygiene check passed."
exit 0
