FROM docker.io/library/alpine:3.20
# The disposable bootstrap controller image (tier-4 fleet helper — the
# committed pin ghcr.io/example/top/topctl:bootstrap-pinned has no real
# registry; this build supplies it for dokku-in-docker environments).
#
# ENTRYPOINT is the daemon (`topctl controller run`, internal/cli).
# `dokku` is a PATH shim (dokku-shim, tier-3 e2etest's runner shape): the
# daemon drives the VM's dokku through the mounted host-daemon socket —
# a dokku-in-docker controller container has no dokku CLI of its own.
COPY topctl /usr/local/bin/topctl
COPY dokku-shim /usr/local/bin/dokku
RUN chmod 0755 /usr/local/bin/topctl /usr/local/bin/dokku \
 && apk add --no-cache git curl docker-cli
# NOTE: runs as root deliberately — the /var/lib/top storage mount is
# root-owned 0700 on the VM (installer contract); a non-root USER cannot
# open the SQLite state (tier-4 verified 2026-11-03).
ENTRYPOINT ["/usr/local/bin/topctl", "controller", "run"]
EXPOSE 8080
