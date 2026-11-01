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
| `observability/prometheus-config/prometheus.yml` | Prometheus scrape config, T-403 fills targets. |
| `observability/grafana-provisioning/` | Grafana datasource + dashboard-provider provisioning and the dashboard JSON. |

Namespace semantics per ADR-0004: `core` reconciles first, `observability`
second; each maps to the Dokku app `<namespace>-<name>` (e.g.
`observability-grafana`). `core` is never drift-pruned (ADR-0004).

## Placeholders & the fictional discipline

- `{{CONTROLLER_IMAGE}}` is the template's **only existential placeholder**,
  rendered at seed time (`internal/install/repo_init.go`). No other file
  carries a `{{…}}` token, and nothing else is render-time-filled.
- Every other suspicious-looking value (`sha256:0000…`, `sha256:1111…`,
  `top.example.com`, `grafana.example.com`, `TODO-T403:…` targets) is
  **fictional documentation**, safe to commit (ADR-0014). Operators replace
  the `*.example.com` domains with their own and pin real digests. The
  digests are deliberately all-zero-or-repeated hex: they satisfy the
  validator's digest syntax check (`sha256:<64 lowercase hex>`) while being
  obviously non-runnable, so nobody accidentally deploys them.
- Grafana's password is **never** in any file: the secret is provisioned
  out-of-band with `topctl secrets set grafana-admin-password` (ADR-0008)
  and referenced by name in `observability/grafana.yaml`.

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
- **`storage`** — named volumes with absolute `mountPath` and an advisory
  `size`. Size values are flexible plain quantities (`2Gi`) — never
  decimals.
- **`env`** — plain string values only, keys matching
  `[A-Za-z_][A-Za-z0-9_]*`. Anything sensitive MUST NOT live here
  (ADR-0008): `GF_SECURITY_ADMIN_PASSWORD` belongs to `secretRefs`.
  Values must be YAML strings when needed (`GF_USERS_ALLOW_SIGN_UP:
  "false"`).
- **`secretRefs`** — names resolved by the configured SecretProvider (v0:
  host-file store, `topctl secrets set`) then injected as env at apply
  time. Names use service-name syntax (`[a-z0-9-]`).
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
| Prometheus retention | **target** 7d time + 1GiB size cap (ADR-0010) | CLI flags (`--storage.tsdb.retention.time=7d --storage.tsdb.retention.size=1GiB`) — **T-403**, not expressible as v0 spec fields. |
| Scrape interval | 15s scrape / 30s eval | gentle on an LP-core host, ample for change detection. |
| Health interval | 30s | Dokku default-ish, low probe noise. |

Tuning knob: retention is *env-configurable at the Prometheus spec* only the
day argument-injection mechanics land (ADR-0010 open question) — until then
edit via the mechanics T-403 defines.

## T-403 handoff (what a later wave must fill)

Everything below is deliberately NOT wired in this template; the files
define only the SHAPE:

1. **Prometheus config delivery**: mount `prometheus-config/prometheus.yml`
   read-only at `/etc/prometheus/prometheus.yml`; pass
   `--config.file=/etc/prometheus/prometheus.yml` plus the retention flags
   listed above as container args (mechanics: Dokku docker-options /
   process-type — pick one during T-403 and document the choice here).
2. **Scrape targets**: replace the three `TODO-T403` targets in
   `prometheus-config/prometheus.yml` with real container network addresses
   (`top-controller`, `node-exporter`, `cadvisor` jobs).
3. **Grafana datasource URL**: replace the `TODO-T403` url in
   `grafana-provisioning/datasources/prometheus.yaml` with the prometheus
   app's container network address.
4. **Storage host-paths**: bind each spec `storage` entry to a TOP-owned
   host path under `/var/lib/top` (ADR-0005 naming discipline); the spec
   declares container mount points only.
5. **Exporter host mounts** (node-exporter /proc + /sys; cAdvisor docker
   socket + cgroups) via read-only Dokku mounts; the v0 spec has no field
   for this, so it lands in T-403 mechanics or a contract extension.
6. **secretRef injection mapping**: how the resolved value of
   `grafana-admin-password` becomes `GF_SECURITY_ADMIN_PASSWORD` (name
   uppercasing convention vs explicit alias) — document the chosen rule in
   the platform README and the reconciler.

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
