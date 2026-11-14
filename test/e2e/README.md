# test/e2e — tier-4 (VM) acceptance scripts

Full-acceptance e2e for TOP's scripting tier (ADR-0012 tier 4:
`docs/architecture/testing-strategy.md`). Everything here runs ON a
disposable VM with a real Dokku — **never** on the author's server and
never against a production host. Tier 1 (hermetic helper tests) live
beside the scripts (`lib_test.go`) and run on every PR.

## Scripts

| Script | Covers | Backlog |
| --- | --- | --- |
| `t404-webhook-poll.sh` | M4 acceptance 3+4: push → provider webhook → converged (actor=webhook op record, app-level deploy evidence, file:line drift origin); then the deliberately-dropped webhook → poll repairs convergence with no webhook evidence on the record | T-404 |
| `t504-kill-deploy.sh` | M5 acceptance 7 (failure-injection matrix, kill-mid-deploy): `docker kill` the controller the instant our push's op row is armed on the record surface ⇒ old release still serves (value + serving-container fingerprint), no completed row while dead, revive (restart policy; `dokku ps:restart` backstop) re-diffs the same sha to a fresh succeeded row, killed run's non-terminal row present then superseded | T-504 |
| `t504-reboot.sh` | Failure-injection matrix "Host reboot": container-set-level restart (the whole managed set's fingerprints in one ledger) or a real VM reboot under `TOP_E2E_REBOOT_VM=1` ⇒ full set running again ≤180s, `topctl doctor` clean post-revival, **no redeploys** (container-set fingerprints stable), poll converges a fresh post-reboot push | T-504 |

Tier-3 twins (HMAC/dedup handler math, adapter apply semantics) live in
`internal/backend/dokku/e2etest` under the `e2e` build tag.

## Running on a VM

Preconditions (asserted, not assumed — check the script headers for the
exact lists):

1. Disposable VM: Docker + Dokku installed; **TOP installed green**
   (`topctl install <repo> --confirm`, M4 acceptance 2 reconciled:
   `core` + `observability` up).
2. The platform repo as Git (URL or checkout) reachable from the VM.
3. Tools on PATH: `jq`, `openssl`, `curl`, `git`, `docker`, `dokku`, `topctl`.
4. The webhook secret file readable by the invoking user:
   `/var/lib/top/secrets/webhook-hmac` (the SecretProvider's default name
   for the controller's HMAC key, internal/webhook/README.md).

Run (fictional values; replace with the operator's own — the platform
repo template in `internal/install/platformrepo` names all the shape):

```sh
TOP_E2E_GIT_BRANCH=main
TOP_E2E_WEBHOOK_URL=https://top.example.com/webhooks/github   # the controller's proxy-routed endpoint
sh test/e2e/t404-webhook-poll.sh  https://git.example.com/ops/top-platform.git
sh test/e2e/t504-kill-deploy.sh   https://git.example.com/ops/top-platform.git
```

Environment knobs (all optional unless marked): `TOP_E2E_GIT_BRANCH`
(default `main`), `TOP_E2E_POLL_SECONDS` (default 60 — keep in sync with
the controller's `controller.pollIntervalSeconds`), `TOP_E2E_WEBHOOK_URL`
(T-404, required), `TOP_E2E_WEBHOOK_SECRET_FILE`
(`/var/lib/top/secrets/webhook-hmac`), service-name overrides
(`TOP_E2E_GRAFANA_APP`, `TOP_E2E_CONTROLLER_APP`,
`TOP_E2E_PROM_APP`, `TOP_E2E_NODE_APP`, `TOP_E2E_CADV_APP`,
`TOP_E2E_USER_APP` for the optional tenant-app survival check,
`TOP_E2E_PROBE_FILE`, `TOP_E2E_PROBE_KEY`), `TOPCTL_BIN`,
`DOKKU_BIN`, `DOCKER_BIN`, plus the T-504 knobs:
the t504-reboot mode pair
`TOP_E2E_REBOOT_VM` + `TOP_E2E_REBOOT_SKIP_REBOOT` (the real-VM reboot
is invoked under your supervisor; re-run the script in verify mode:
issue the reboot, wait, then re-run with `TOP_E2E_REBOOT_SKIP_REBOOT=1`).

## What actually executes (evidence surfaces)

- **Webhook delivery** is GitHub-convention HMAC
  (`X-Hub-Signature-256` + `X-GitHub-Event: push` + `X-GitHub-Delivery`)
  against `POST /webhooks/github` — the exact semantics
  `internal/webhook` fixes, so a script delivery is indistinguishable
  from the real provider's.
- **Convergence evidence** comes from the record + live surfaces, never
  from logs: `topctl history -o json` (op rows: `actor`, `revision`,
  `correlationId`, `state`), `topctl diff` (exit 0 clean / drift rows
  carrying the `file:line` spec origin), and dokku itself
  (`dokku config:get <app> <key>` for the applied spec env probe,
  `docker inspect` for container/image identity).
- **The spec change** is a probe env item injected under the target
  service's `env:` mapping (`lib.sh`'s `inject_env_probe`) — additive,
  rollback-free, and visible in every evidence surface above. Use a FRESH
  probe key per scenario (e.g. `TOP_E2E_PROBE_KEY=E2E_PROBE_T404` then a
  distinct `..._T404_S2`): env convergence is KEY-SET based by design
  (the adapter never reads config values, ADR-0008), so a second
  VALUE-change on the same key would never converge and the
  `config:get` value assertions would hang (tier-4 verified
  2026-11-03).

## Disposable dokku-in-docker VM (fleet-run shape)

The scripts don't care where the VM comes from; the committed helper
`dokku-vm.sh` + `controller-bootstrap/` assemble one on any Docker host
(MacOS Docker Desktop works):

- `test/e2e/dokku-vm.sh` boots the dokku/dokku container with the
  HOST-daemon socket mount (the tier-3 e2etest shape), copies in
  `topctl`+`jq`, starts a git-daemon over the seeded platform repo
  (`git://host.docker.internal:9418/...` — installer-validateRepoURL's
  accepted scheme), publishes 9418, and runs `topctl install --init`.
  The shared state dir is a dockerd-side bind
  (`/var/lib/e2e-top-dk` symlinked from `/var/lib/top` on the daemon
  filesystem): the CLI (inside the VM), the controller container (via
  top-metadata storage mount), and every dokku storage bind resolve to
  ONE sqlite / secret dir. On the dockerd side only the symlink needs
  making once (the VM container's bind resolves through it).
- `test/e2e/controller-bootstrap/` builds the pinned
  `ghcr.io/toledompm/top/topctl:v0.1.0` locally (no registry
  exists): `topctl controller run` entrypoint, docker-cli, `git`, and a
  `dokku` PATH shim (`docker exec -i top-e2e-vm dokku`) — a controller
  container in a dokku-in-docker topology has no dokku CLI of its own;
  the shim mirrors the tier-3 harness's docker-exec runner. It needs
  `-v /var/run/docker.sock` via `dokku docker-options:add … deploy`.
  The image runs as root: state/secret dirs are root-owned 0700.
- Operator work between install and acceptance-2 green: replace the
  template's FICTIONAL digest stand-ins with real arm64 digests in the
  seeded repo, provision `<dataDir>/secrets/{webhook-hmac,
  gf-security-admin-password}` (0600), drop the T-311 ports workaround
  (ports entries livelock; healthcheck blocks validate against declared
  ports, so omit both), and set the controller's TOP_GIT_URL
  (`dokku config:set … --no-restart` — nothing wires it today).

## Helper tests (tier 1)

`lib_test.go` runs the pure helpers of `lib.sh` under a real `sh`
(assertion/`wait_until` behavior, the probe-edit idempotency and
refusals, the HMAC byte-for-byte equivalence with
`internal/webhook.verify`'s Go computation, and the
`docker_running_name` contract against a stub binary). Included in the
normal `go test ./...` gate — no env flag.
