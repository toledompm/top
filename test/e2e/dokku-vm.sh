#!/bin/sh
# dokku-vm.sh — the disposed dokku-in-docker tier-4 testbed bootstrap
# (fleet-run helper, tier-4 e2e only; fleet-verified 2026-11-03).
# SPDX-License-Identifier: MIT
#
# Provisions ONE disposable dokku/dokku container as the "VM" (host-daemon
# socket mount, the tier-3 e2etest shape), installs TOP inside it from the
# committed repo via `topctl install --init` against a disposable git-daemon,
# and prints the operator hand-off state. Idempotent across re-runs: every
# step re-checks before mutating.
#
# Host preconditions: docker daemon (Docker Desktop on macOS ok), this repo
# checked out; binaries topctl/jq under $BIN_DIR (build:
#   GOOS=linux GOARCH=<docker arch> go build -o "$BIN_DIR"/topctl ./cmd/topctl
#   curl -LsS jq-linux-arm64 -> "$BIN_DIR"/jq
# and the controller bootstrap image:
#   docker build -t ghcr.io/example/top/topctl:bootstrap-pinned test/e2e/controller-bootstrap
#
# Everything below creates ONLY containers/images named like *top-e2e-vm* /
# *ghcr.io/example/top/topctl* and a git daemon inside the VM container.
set -u

VM=top-e2e-vm
REPO_PORT=9418
REPO_URL="git://host.docker.internal:${REPO_PORT}/platform-repo.git"
SHARED=/var/lib/e2e-top-dk # dockerd-visible host dir backing <DataDir>
BUNDIR=${BIN_DIR:?BIN_DIR=/path/to/binaries (topctl + jq linux binaries)}
TAB=$(docker image inspect docker.io/dokku/dokku:latest --format '{{.Architecture}}') # arm64/amd64

docker rm -f "$VM" >/dev/null 2>&1 || true
docker run -d --name "$VM" --add-host=host.docker.internal:host-gateway \
    -p "$REPO_PORT":9418 -v /var/run/docker.sock:/var/run/docker.sock \
    -e DOKKU_HOSTNAME=deploy.example.internal \
    -v /var/lib/e2e-top-dk:/var/lib/top \
    docker.io/dokku/dokku:latest >/dev/null
# first-start bootstrap; HealthyBackend equivalent: dokku answers AND ops work.
i=0; while [ $i -lt 60 ]; do
    if docker exec "$VM" sh -c 'dokku version && docker info' >/dev/null 2>&1; then break; fi
    sleep 3; i=$((i+3))
done
docker exec "$VM" sh -c 'dokku version && docker info --format daemon-ok' || exit 1

# in-VM tools: topctl + jq (the dokku image already ships git/curl/openssl).
docker cp "$BUNDIR/topctl" "$VM":/usr/local/bin/topctl
docker cp "$BUNDIR/jq" "$VM":/usr/local/bin/jq
docker exec "$VM" chmod 0755 /usr/local/bin/topctl /usr/local/bin/jq

# disposable git-daemon repo inside the VM (--receive-pack enabled so the
# scripts' own pushes work over git://).
docker exec "$VM" sh -c '
    rm -rf /srv/top-e2e && mkdir -p /srv/top-e2e
    git init -q --bare /srv/top-e2e/platform-repo.git
    git -C /srv/top-e2e/platform-repo.git symbolic-ref HEAD refs/heads/main'
# NOTE: -d (detached exec): a backgrounded nohup inside a FOREGROUND exec
# dies with the exec session (tier-4 verified 2026-11-03).
docker exec -d "$VM" /usr/lib/git-core/git-daemon --base-path=/srv/top-e2e \
    --export-all --enable=receive-pack --reuseaddr --port=9418
sleep 2
docker exec "$VM" git ls-remote "$REPO_URL" >/dev/null 2>&1 || { echo "git-daemon not reachable"; exit 1; }

# preflight material: a throwaway operator key pair and ghost known_hosts
# (git:// remotes never use them — the installer's operator paths are the
# v0 requirement).
docker exec "$VM" sh -c '
    ssh-keygen -t ed25519 -N "" -f /root/keydown -q 2>/dev/null || true
    : > /root/known_hosts
    mkdir -p /var/lib/top/secrets && chmod 700 /var/lib/top/secrets
    openssl rand -hex 32 > /var/lib/top/secrets/webhook-hmac
    chmod 600 /var/lib/top/secrets/webhook-hmac'

# THE install (unit 2's shape): --init seeds the embedded template; phase 5
# runs the first reconcile with the REAL operator digests next.
docker exec -e TOP_GIT_URL="$REPO_URL" -e TOP_GIT_BRANCH=main -e TOP_INIT_TOKEN=dummy-disposable-token "$VM" \
    topctl install "$REPO_URL" --init --confirm --skip-ports \
    --deploy-key /root/keydown --known-hosts /root/known_hosts 2>&1 | tail -6

# the daemon's own config: nothing wires this today (open seam, see the
# fleet report); the operator supplies the controller env + the canonical
# config file for every CLI shell.
docker exec -e REPO_URL="$REPO_URL" "$VM" sh -c '
    # The daemon drives the VM dokku through the socket-mounted shim:
    # the CONTAINER (an ordinary host-daemon deployment) needs the host
    # docker socket to reach `docker exec ... top-e2e-vm dokku` (tier-4
    # verified 2026-11-03: without it every poll run failed at ensure).
    dokku docker-options:add core-top-controller deploy \
        "-v /var/run/docker.sock:/var/run/docker.sock" >/dev/null
    dokku ps:rebuild core-top-controller >/dev/null
    dokku config:set core-top-controller TOP_GIT_URL="$REPO_URL" TOP_GIT_BRANCH=main --no-restart >/dev/null
    dokku ps:restart core-top-controller >/dev/null
    cat > /var/lib/top/top-config.yaml <<EOF
apiVersion: top/v1alpha1
git:
  url: "$REPO_URL"
  branch: main
EOF
    chmod 600 /var/lib/top/top-config.yaml'
echo "booted: $VM — install recorded; wait one poll then run the adopt assertions."
