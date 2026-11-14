#!/bin/sh
# bootstrap-repo.sh — T-600: the operator flow for steps 1-3 of the
# platform-repo setup, ON the host that will run `topctl install`:
#
#   1. a deploy keypair in ~/.ssh (generated if missing) + pinned
#      known_hosts,
#   2. the private GitHub repo (created via `gh` when available;
#      otherwise a direct deep-link URL is printed and the script WAITS
#      for the operator to confirm the repo exists),
#   3. the deploy key registered read-write (deep-link + wait when gh is
#      unavailable — the browser-only page nothing can automate),
#   4. an ssh reachability check against GitHub.
#
# Runs interactively BY DESIGN: every wait is an explicit operator
# checkpoint, never a prompt-hang (BatchMode discipline applies to
# topctl's own transports, not this human setup).
# SPDX-License-Identifier: MIT
set -eu

REPO_ARG=${1:-}
[ -n "$REPO_ARG" ] || { echo "usage: $0 <owner/repo>  (e.g. toledompm/top-homelab)"; exit 2; }
case "$REPO_ARG" in */*) ;; *) echo "FAIL: <owner/repo> must include the owner"; exit 2;; esac

SSHDIR="$HOME/.ssh"
KEY="$SSHDIR/top_deploy_key"
KH="$SSHDIR/top_known_hosts"
mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"

# 1. keypair + pinned hosts
if [ ! -f "$KEY" ]; then
    ssh-keygen -t ed25519 -f "$KEY" -N "" -C "top-platform-repo" -q
    echo "▸ generated deploy key $KEY"
else
    echo "▸ deploy key already exists: $KEY"
fi
if [ ! -s "$KH" ]; then
    ssh-keyscan -t ed25519 github.com 2>/dev/null > "$KH"
    echo "▸ pinned github.com host keys → $KH"
fi

# 2. repo (gh when present; deep-link + wait otherwise)
REPO_URL="https://github.com/$REPO_ARG"
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    if gh repo view "$REPO_ARG" >/dev/null 2>&1; then
        echo "▸ repo exists: $REPO_URL"
    else
        gh repo create "$REPO_ARG" --private
        echo "▸ created private repo $REPO_URL"
    fi
else
    echo "────────────────────────────────────────────────────────────"
    echo "gh is unavailable — create the PRIVATE repo by hand:"
    echo "  $REPO_URL/new?name=${REPO_ARG#*/}   (empty, no README)"
    printf "Press ENTER once the repo exists: "
    read -r _
    gh repo view "$REPO_ARG" >/dev/null 2>&1 || true
fi

# 3. deploy key (gh api when present; deep-link + wait otherwise)
PUBKEY=$(cat "$KEY.pub")
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
    if gh api "/repos/$REPO_ARG/keys" --jq '.[] | .title' 2>/dev/null | grep -q "^media-server-top$"; then
        echo "▸ deploy key already registered"
    else
        gh api -X POST "/repos/$REPO_ARG/keys" \
            -f title="media-server-top" -f key="$PUBKEY" -F read_only=false >/dev/null
        echo "▸ registered deploy key (read-write; flip to read-only after --init)"
    fi
else
    echo "────────────────────────────────────────────────────────────"
    echo "Add the deploy key by hand (ALLOW WRITE ACCESS for now —"
    echo "flip to read-only after the install's --init push):"
    echo "  key:  $PUBKEY"
    echo "  page: $REPO_URL/settings/keys/new"
    printf "Press ENTER once the key is added: "
    read -r _
fi

# 4. reachability (the exact identity `topctl install` will use)
if ssh -i "$KEY" -o IdentitiesOnly=yes -o UserKnownHostsFile="$KH" \
        -o BatchMode=yes -o ConnectTimeout=10 -T git@github.com 2>&1 | grep -qE "successfully authenticated|does not provide shell access"; then
    echo "✓ GitHub reachable with the deploy key"
else
    echo "FAIL: GitHub did not accept the deploy key (check step 3: read-write, right repo)"; exit 1
fi

echo "────────────────────────────────────────────────────────────"
echo "Next (on this host):"
echo "  sudo topctl install git@github.com:$REPO_ARG.git \\"
echo "    --init \\"
echo "    --deploy-key $KEY \\"
echo "    --known-hosts $KH"
echo "After the install goes green: flip the deploy key to READ-ONLY"
echo "($REPO_URL/settings/keys)."
