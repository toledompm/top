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
| `Inspect(ns, name)` | Structured actual state: deployed?, running?, revision/image, env keys, ports, domains, storage mounts, restart policy, resources, networks, last build status | `ps:report --format json`, `builds:report`, `git:report --git-sha/--git-source-image`, `apps:report --app-deploy-source(-metadata)`, `ports:report --ports-map-json`, `storage:list --format json`, `proxy:report --format json`, `network:report`, `resource:report` |
| `Apply(spec, secrets)` | Converge one service to its ServiceSpec. Batches env via `--no-restart`; deploys per ADR-0002; sets ports/domains/storage/networks/restart-policy/resources; zero-downtime by default | `config:set --no-restart`, `git:from-image [--force]`, `git:sync --build-if-changes`, `git:from-archive -- tar` (stdin, for synthesized app.json), `ports:set`, `domains:set`, `storage:create`/`storage:mount`, `network:set`, `ps:set restart-policy`, `resource:limit`, `checks:set` |
| `Readiness(ns, name)` | Is the applied revision live and healthy? | composed: `ps:report --deployed --running` + `builds:report --build-status` + `checks:run` (active probe) |
| `LastGoodRevision(ns, name)` | What was last successfully deployed? | `git:report --git-source-image` / `apps:report --app-deploy-source-metadata` |
| `Stop(ns, name)` / `Destroy(ns, name)` | Safe stop; destroy only from explicit prune/uninstall paths with ownership already proven by caller | `ps:stop`; `apps:destroy --force` |
| `FailedLogs(ns, name)` | Last failed deploy output (redacted) | `logs:failed`, `builds:output` |
| `EnsureNetwork(name)` | Shared internal network for `routing.mode: internal` | `network:create <name>` (carries `com.dokku.network-name`, attachable) |
| `HealthyBackend()` | Is Dokku itself usable? | `dokku version` + a cheap `apps:list --format json` |

## Error semantics

- Typed errors: `ErrNotFound`, `ErrConflict` (app exists, not ours),
  `ErrBackend` (dokku non-zero exit, carries redacted stderr + exit code),
  `ErrUnhealthy` (backend unusable).
- Dokku exit 0 with stderr warnings → log, do not fail.
- Timeouts per command class (reports 30 s, deploys 15 min, config 60 s);
  context cancellation always honored; `WaitDelay` set so pipes can't wedge.

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
