# Contract: PlatformBackend

The runtime abstraction over the workload engine. One implementation in v0:
`internal/backend/dokku` — the **only** package that knows Dokku command
syntax. Boundary rules from ADR-0001 apply: dokku CLI only, never direct
Docker API mutation of apps, never `dokku`/`com.dokku.*` labels on TOP things.

## Responsibilities & operations

(Names are illustrative; final Go signatures are derived in Milestone 1.
All methods take `ctx`, respect cancellation, and have per-class timeouts.)

| Operation | Semantics | Dokku mapping (verified, 0.38.x) |
| --- | --- | --- |
| `EnsureApp(ns, name)` | Create app if absent; adopt if exists-and-ours | `apps:exists` (exit code) → `apps:create <ns>-<name>` |
| `Inspect(ns, name)` | Structured actual state: deployed?, running?, revision/image, env KEY NAMES only (values never read — ADR-0008), ports, storage mounts, restart policy, resources, networks, last build status. **Domains are NOT populated yet** (no verified `domains:report` JSON on 0.38) | `ps:report --format json`, `builds:report`, `git:report`, `apps:report`, `ports:report --ports-map-json`, `storage:list --format json`, `config:keys`, `network:report`, `resource:report` (all `--format json` where offered) |
| `Apply(ref, spec, secrets, opts)` | Converge one service to its ServiceSpec. Env converges by KEY SET (at most one `config:set --no-restart` + one `config:unset`; value drift deliberately not compared). Resource specs are K8s-style quantities translated to docker-native before `resource:limit`, compared semantically (converged apps never re-mutate). Deploys per ADR-0002; zero-mutation skips require the release to actually be running; `--force` on mutable tags only when movement was observed. Archive (healthcheck) deploys fingerprint their app.json into the adapter-owned `TOP_ARCHIVE_SHA256` env key, written only after a successful deploy (ADR-0002 amendment A1). `checks:set` is NOT used for timings — vacated on 0.38 (`wait-to-retire` is the only accepted key; timings ride the app.json) | `config:set --no-restart`, `config:unset`, `git:from-image [--force]`, `git:sync --build-if-changes`, `git:from-archive <app> -- tar` (via stdin), `ps:rebuild`, `ports:set`, `domains:set`, `storage:create`/`storage:mount`/`storage:unmount`, `network:set`, `ps:set restart-policy`, `resource:limit [--cpu --memory --process-type web]` / `resource:limit-clear` |
| `Readiness(ref)` | Composes from reports: deployed + running + last build status, plus an ACTIVE `checks:run` probe (skipped — reported healthy — when nothing is running to probe). A failing check is the ANSWER (`Checks=false`), never an operation failure | `ps:report` / `builds:report` (via Inspect compose) + `checks:run` |
| `LastGoodRevision(ns, name)` | What was last successfully deployed? | `apps:report --app-deploy-source-metadata`, fallback `git:report --git-source-image` |
| `Stop(ns, name)` / `Destroy(ns, name)` | Safe stop; destroy only from explicit prune/uninstall paths with ownership already proven by caller | `ps:stop`; `apps:destroy --force` |
| `FailedLogs(ref, lines)` | Last failed deploy output (redacted upstream), split to lines, most-recent `lines` kept. Missing app → `ErrNotFound`; exit 1 (no failed-deploy output) → empty success | `logs:failed <app>` |
| `ServiceLogs(ref, lines)` (T-308 extend, not fork) | Recent app output for `topctl logs` runtime mode: same shape as FailedLogs (most-recent-last; exit 1 = empty) | `logs <app> [--num N]` |
| `EnsureNetwork(name)` | Shared internal network for `routing.mode: internal` | `network:exists` → `network:create <name>` |
| `HealthyBackend()` | Is Dokku itself usable? | `dokku version` + a cheap `apps:list --format json` |

## Error semantics

- Typed errors: `ErrNotFound`, `ErrConflict` (app exists, not ours),
  `ErrBackend` (dokku non-zero exit, carries redacted stderr + exit code),
  `ErrUnhealthy` (backend unusable).
- Dokku exit 0 with stderr warnings → log, do not fail.
- Timeouts per command class as implemented (reports 30 s, deploys 10 min,
  mutations 2 min, destroy 5 min); context cancellation always honored;
  `WaitDelay` set so pipes can't wedge. A dokku not-exist answer is absent
  via exit 20 (0.38.32 canonical, tier-3 verified), with exit 10/1 as
  loose aliases.

## Mapping notes & known wrinkles (from ADR-0001/0002 research)

- `config:set` redeploys by default → always `--no-restart` in batches.
- `git:from-image` skips pulls when the tag exists locally → pass `--force`
  when the desired ref is a mutable tag and poll detected movement; digests
  never need it.
- Custom healthchecks for image deploys go through the synthesized-archive
  path (`app.json` `healthchecks.web`); validate end-to-end in Milestone 3
  and fall back to Dokku default checks with a loud warning if the path
  misbehaves.
- `checks:report` has no documented JSON output → do not build on it; use
  `checks:run` for active verification.
- Storage host paths are never auto-created on mount → `EnsureApp` flow
  creates entries first (`storage:create`), then mounts, then restarts.
- `apps:destroy` detaches storage but leaves entries/host dirs (docs silent;
  source-confirmed) → prune flows decide data lifecycle explicitly.

## Illustrative interaction

```
Apply(prometheus-spec):
  EnsureApp → apps:create observability-prometheus
  storage:create observability-prometheus-data --chown root  (idempotent check via storage:list-entries)
  storage:mount observability-prometheus observability-prometheus-data:/prometheus
  network:set observability-prometheus attach-post-deploy top-internal
  config:set --no-restart observability-prometheus TZ=UTC
  git:from-image observability-prometheus docker.io/prom/prometheus@sha256:…
  Readiness: ps:report --deployed → builds:report --build-status=succeeded → checks:run
```
