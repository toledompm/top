# TOP platform repo — example fleet (T-401)

This repository is TOP's GitOps source of truth (ADR-0002): every
TOP-managed workload is a ServiceSpec at `<namespace>/<name>.yaml`
([contract](../../../docs/contracts/service-spec.md)); the controller
converges the host to the specs at the pinned revision. The template content
is embedded in `topctl` and committed by `topctl install --init` when the
configured platform repo resolves empty (ADR-0005).

One source of truth: `examples/platform-repo` is a **symlink** to
`internal/install/platformrepo` — edit only `internal/install/platformrepo`.
(NB: the spec loader never resolves symlinks during a repo walk, so shipped
repo trees must contain the real files — the symlink exists only inside the
TOP source repo for browsing.)

## Layout

| Path | What it is |
| --- | --- |
| `core/top-controller.yaml` | TOP's controller, self-managed like any workload (deploy human-gated, ADR-0005: `topctl deploy core/top-controller`). |
| `observability/prometheus.yaml` | Metrics TSDB (ADR-0010). |
| `observability/grafana.yaml` | Dashboards; admin password via `secretRefs` (ADR-0008); the first real secretRef consumer. |
| `observability/node-exporter.yaml` | Host metrics. |
| `observability/cadvisor.yaml` | Per-container metrics (Dokku apps are plain containers — justified, ADR-0010). |
| `observability/prometheus-config/prometheus.yml` | Prometheus scrape config (targets wired, T-403). |
| `observability/grafana-provisioning/` | Grafana datasource + dashboard-provider provisioning and the dashboard JSON (synced into the provisioning entry dir each reconcile). |

Namespace semantics per ADR-0004: `core` reconciles first, `observability`
second; each maps to the Dokku app `<namespace>-<name>` (e.g.
`observability-grafana`). `core` is never drift-pruned (ADR-0004).

## Placeholders & the fictional discipline

- `{{CONTROLLER_IMAGE}}` is the template's **only existential placeholder**,
  rendered at seed time (`internal/install/repo_init.go`). No other file
  carries a `{{…}}` token, and nothing else is render-time-filled.
- Every other suspicious-looking value (`sha256:0000…`, `sha256:1111…`,
  `top.example.com`, `grafana.example.com`) is **fictional documentation**,
  safe to commit (ADR-0014). Operators replace
  the `*.example.com` domains with their own and pin real digests. The
  digests are deliberately all-zero-or-repeated hex: they satisfy the
  validator's digest syntax check (`sha256:<64 lowercase hex>`) while being
  obviously non-runnable, so nobody accidentally deploys them.
- Grafana's password is **never** in any file: the secret is provisioned
  out-of-band into the host secret store (`/var/lib/top/secrets/<name>`,
  0600 file — until a `topctl secrets` verb lands) under the name
  `gf-security-admin-password` (the dash-kebab form of the env var it
  feeds — see item 6 below) and referenced by name in
  `observability/grafana.yaml`.

## Field-by-field (why each field is what it is)

- **`source.image` + `source.digest`** — immutable revisions contract line:
  digest-pinned, never mutable tags (the platform repo should raise zero
  mutable-tag warnings). Aliases: `tag` (mutable, warns) or
  `source.build` (Dokku source build); exactly one source mode allowed.
- **`ports`** — named declarations; healthchecks reference them by name.
  `hostPort` exists in the contract but is discouraged; the fleet relies on
  routing + container networking instead.
- **`routing`** — `internal` (container network only) for prometheus /
  node-exporter / cadvisor: operator-facing tools that shouldn't be
  internet-reachable (ADR-0010: diagnosing TOP never requires the
  monitoring stack ON the internet). `proxy` for grafana (dashboards at
  your domain) and the controller (webhooks must reach it from Git
  providers per ADR-0003). proxy + domains are validated together — proxy
  with an empty domains list is a validation error (contract).
- **`healthcheck`** — maps to Dokku checks; the endpoints are the real
  ones: `/healthz` on the controller (:8080, internal/controller),
  `/-/healthy` for prometheus/node-exporter, `/healthz` for cadvisor,
  `/api/health` for grafana. `intervalSeconds: 30` (small host, gentle
  probing). An absent healthcheck = Dokku defaults.
- **`storage`** — named entries with absolute `mountPath` and an advisory
  `size`. Size values are flexible plain quantities (`2Gi`) — never
  decimals. The reconciler binds each entry to a TOP-owned host dir
  `<VolumeRoot>/<app>-<entry>` (app = `<ns>-<name>`, ADR-0009) and
  `config`/`provisioning` entries additionally get the repo's matching
  provisioning tree synced into them each run (see "T-403 wiring" item 1/4
  + internal/reconcile/provision.go).
- **`env`** — plain string values only, keys matching
  `[A-Za-z_][A-Za-z0-9_]*`. Anything sensitive MUST NOT live here
  (ADR-0008): `GF_SECURITY_ADMIN_PASSWORD` belongs to `secretRefs`.
  Values must be YAML strings when needed (`GF_USERS_ALLOW_SIGN_UP:
  "false"`).
- **`secretRefs`** — names resolved by the configured SecretProvider (v0:
  host-file store, `/var/lib/top/secrets/<name>` 0600) then injected as
  env at apply time under the dash-kebab↔env-var rule (item 6 below).
  Names use service-name syntax (`[a-z0-9-]`).
- **`dependsOn`** — intra-namespace only (cross-namespace is a validation
  error; namespace tiers are the cross-cutting order). Only grafana →
  prometheus is declared: datasource target exists before grafana deploys.
- **`rollout.maxUnavailable`** — omitted ⇒ default 0: zero-downtime checks
  must pass before a rollout proceeds.
- **`restartPolicy: always`** — observability independence (ADR-0010): the
  stack keeps running whether or not TOP does.
- **`resources.limits`** — small-host defaults below.
- **`annotations.top/special: bootstrap`** — the human-gated self-update
  marker for the controller (ADR-0005).

## Small-host defaults (and why)

| Choice | Value | Why |
| --- | --- | --- |
| Controller limits | 250m / 256Mi | reconcile runs are short bursts; ¼ core + 256Mi keeps it off the tenant budget. |
| Prometheus limits | 500m / 512Mi | the heaviest fleet app; 15s scrape, small retention bound. |
| Grafana limits | 250m / 256Mi | one operator; 6h default query range. |
| node_exporter | 100m / 64Mi | pure metric scraping. |
| cAdvisor | 200m / 128Mi | container introspection every scrape. |
| Prometheus retention | **target** 7d time + 1GiB size cap (ADR-0010) | Not wired: the CLI flags need a container-args surface the v0 spec lacks (see "T-403 wiring" item 2). |
| Scrape interval | 15s scrape / 30s eval | gentle on an LP-core host, ample for change detection. |
| Health interval | 30s | Dokku default-ish, low probe noise. |

Tuning knob: retention stays at the image default (15d time, no size cap)
until item 2's `spec.args` extension or the operator interim; the spec's
`storage.data: size: 2Gi` is the space reserve that keeps a default-run
harmless on the small host.

## T-403 wiring (resolved mechanics)

Each of the six handoff items, with the mechanism VERIFIED against the
v0 contract + adapter (internal/backend/dokku, reconcile VolumeRoot) and
the deviations recorded for the owner to approve (AGENTS.md rule 3:
durable decisions belong in ADRs — these are PROPOSALS, not decisions):

1. **Prometheus config delivery — storage entry, no args needed.** The
   spec's `storage: {name: config, mountPath: /etc/prometheus}` rides the
   reconciler's existing VolumeRoot mechanics: entry `observability-
   prometheus-config` at
   `<VolumeRoot>/observability-prometheus-config`, mounted via
   `storage:mount <app> <entry> --container-dir /etc/prometheus`
   (ADR-0009: entries created
   `--chown false` under TOP's VolumeRoot — never /var/lib/dokku).
   `/etc/prometheus/prometheus.yml` is the prom/prometheus image's
   DEFAULT `--config.file`, so **no `--config.file` argument is needed**
   — the spec cannot express container args, and this choice avoids
   every one. The POPULATION half is the controller's provisioning sync:
   each reconcile copies `observability/prometheus-config/` into the
   entry dir (hash-changed files only; stale files removed — the entry
   dir is TOP-owned, GitOps semantics), BEFORE any apply mounts. Why
   sync instead of mounting the repo checkout directly: gitsource
   materializes revisions into EPHEMERAL temp dirs and never a shared
   checkout, so only a copied, stable VolumeRoot path may back a
   dokku-mounted entry. A config edit reaches the container file
   immediately through the bind mount but needs a prometheus
   reload/restart to be loaded.
2. **Retention flags — NOT wired; smallest additive proposal.** The v0
   spec has no container-args field (`spec.command`/`spec.args`), env
   cannot carry CLI flags, and dokku docker-options cannot inject CMD
   (options precede the image in `docker run`). Proposal (needs spec +
   domain + plan + adapter alignment, owner approval; NOT implemented by
   T-403): `spec.args[]` — argv appended after the image's entrypoint —
   converging via the synthesized-app.json Dockerfile CMD when
   healthchecks ride the archive deploy, and fingerprint-covered so
   arg-only changes re-deploy. Operator interim that works TODAY:
   ship a personal wrapper image (FROM prom/prometheus, CMD with the
   flags) from your own registry and digest-pin it in
   `observability/prometheus.yaml`.
3. **Scrape targets — Dokku network aliases (deterministic).** On any
   dokku-attached docker network, app A / process type P resolves as
   `A.P` (dokku network-management docs — verified alias scheme,
   network-independent). Filled in `prometheus-config/prometheus.yml`:
   `core-top-controller.web:8080` (controller /metrics, port 8080 per
   internal/controller), `observability-node-exporter.web:9100`,
   `observability-cadvisor.web:8080`, `observability-grafana.web:3000`.
   Aliases appear only AFTER healthchecks pass (attach-post-deploy) — a
   `down` target means checks failing, per an intended readiness gate.
4. **Grafana datasource + dashboards — one provisioning tree.**
   Datasource URL `http://observability-prometheus.web:9090` (same alias
   rule; grafana dependsOn prometheus so the target exists first).
   Provisioning delivery rides item 1's sync: `grafana-provisioning/` is
   copied into `<VolumeRoot>/observability-grafana-provisioning` (entry
   `provisioning`, mounted at /etc/grafana/provisioning). ONE tree feeds
   both Grafana read locations: datasources/ at startup, and the
   dashboard provider scans /etc/grafana/provisioning/dashboards
   (provider.yaml) — the old separate dashboard storage entry is gone.
   UI edits persist INTO the volume and are the documented
   operator-lives-here path (allowUiUpdates).
5. **Storage host paths — already the reconciler's semantics; sized for
   retention.** Every spec storage entry binds to
   `<VolumeRoot>/<app>-<entry>` under TOP's VolumeRoot (/var/lib/top/…)
   with `--chown false` (ADR-0009) — spec files declare container mount
   points only. Data sizes: prometheus `data` 2Gi (≥ the 1GiB retention
   target + WAL headroom), prometheus `config` (small tree), grafana
   `data` 1Gi, grafana `provisioning` (small tree). Exporter HOST mounts
   are the NOT-wired deviation: read-only binds of host /proc, /sys,
   /var/run/docker.sock need either Dokku-side storage surfaces (no
   read-only flag exists) or ADR-0001-flagged docker -v — proposal:
   `spec.storage[].readOnly: true` (backend storage:mount is then only
   safe for read-only CONSUMERS once dokku grows the flag; until then
   node-exporter/cadvisor ride the operator docker-options interim
   documented in their spec notes, and their scraped numbers are
   container-local — treat those two jobs as provisional).
6. **secretRef → env mapping — dash-kebab rule.** Chosen convention:
   the secret NAME is the dash-kebab form of the env var it feeds
   (dashes become underscores, then uppercased): secret
   `gf-security-admin-password` injects as `GF_SECURITY_ADMIN_PASSWORD`.
   The reconciler applies the mapping (`internal/reconcile/secretenv.go`,
   `secretEnvKey`) when writing the resolved value via `config:set`;
   plan/vocabulary still reference `secretRefs.<name>` (never the value).
   Operator setup step (until a `topctl secrets` verb exists):
   ```
   printf 'the new admin password' > /var/lib/top/secrets/gf-security-admin-password
   chmod 600 /var/lib/top/secrets/gf-security-admin-password
   ```
   The store refuses loose perms loudly (ADR-0008), diffs the name only.

## Workflow

```
git edit <namespace>/<name>.yaml   →  webhook or poll triggers a run
topctl diff                        →  the plan, before anything applies
topctl reconcile --now             →  converge immediately
topctl deploy core/top-controller  →  human-gated controller self-update (ADR-0005)
```

Deleted spec files are orphaned; removal always needs the explicit
`topctl reconcile --prune` (never for `core` — ADR-0004). Note: the spec
loader walks repo YAML at depth 1 and 2 only, and skips symlinks — so
**never** place another `.yaml` at top level or inside `observability/`
unless it's a ServiceSpec; provisioning YAML lives deeper (that's why
`grafana-provisioning` is a subdirectory tree).
