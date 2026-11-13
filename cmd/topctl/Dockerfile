# cmd/topctl/Dockerfile — the PRODUCT TOP controller image (ADR-0005's
# bootstrap artifact), built + pushed by the CI release job on version
# tags (.github/workflows/release.yml): a Dokku host pulls it at install
# time, so the host never needs a Go toolchain. One binary, one version
# chain (the daemon's top_build_info agrees with the image tag).
#
# `dokku` on PATH is the dokku-shim: the controller container has no dokku
# CLI; the shim drives the HOST's dokku through the Docker socket the
# installer mounts into the app's deploy (docker-options wiring at
# bootstrap, same shape the tier-3/4 e2e harnesses verified 2026-11-03).
# docker-cli + git are the shim's and the gitsource's runtime needs.
# Runs as root deliberately: the /var/lib/top storage mount is root-owned
# 0700 on the host (installer contract); a non-root user cannot open the
# SQLite state (verified live 2026-11-03).
FROM docker.io/library/alpine:3.20
COPY topctl /usr/local/bin/topctl
COPY cmd/topctl/dokku-shim /usr/local/bin/dokku
RUN chmod 0755 /usr/local/bin/topctl /usr/local/bin/dokku \
 && apk add --no-cache git docker-cli
ENTRYPOINT ["/usr/local/bin/topctl", "controller", "run"]
EXPOSE 8080
