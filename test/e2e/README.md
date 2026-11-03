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
| `t405-survival.sh` | M4 acceptance 8: `docker kill` controller ⇒ workloads + monitoring stack keep running; revival via restart policy and via `topctl recover restart`; missed change reconciles on return; **no redeploy** across the exercise (controller image/container fingerprint stable ⇒ git:from-image never re-ran; adoption stays one-shot) | T-405 |

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
sh test/e2e/t405-survival.sh      https://git.example.com/ops/top-platform.git
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
`DOKKU_BIN`, `DOCKER_BIN`.

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

## Helper tests (tier 1)

`lib_test.go` runs the pure helpers of `lib.sh` under a real `sh`
(assertion/`wait_until` behavior, the probe-edit idempotency and
refusals, the HMAC byte-for-byte equivalence with
`internal/webhook.verify`'s Go computation, and the
`docker_running_name` contract against a stub binary). Included in the
normal `go test ./...` gate — no env flag.
