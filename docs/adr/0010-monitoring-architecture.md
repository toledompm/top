# 0010 — Monitoring architecture and its independence from TOP

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

The observability stack is itself a TOP-managed workload (namespace
`observability`), yet it must keep running when TOP fails, and diagnosing TOP
must never require the monitoring stack. The stack must also fit small hosts
(the brief's environment realities).

## Decision

**Stack (all ordinary ServiceSpecs in `observability/`):**

- **Prometheus** — metrics. Scrapes: the TOP controller (`/metrics`),
  node_exporter, cAdvisor, and itself. Retention and scrape config sized for
  small hosts by default (e.g. 7d / 1 GiB cap, configurable via ServiceSpec
  env).
- **Grafana** — dashboards (TOP overview, host, containers). Provisioned from
  ConfigMap-style files mounted via Dokku storage; no manual UI setup needed.
- **node_exporter** — host metrics.
- **cAdvisor** — per-container metrics (justified: Dokku apps are plain
  Docker containers; without cAdvisor there is no per-app resource view).
- **No Alertmanager in v0** — one operator, terminal-first workflow; alerting
  channels are a per-user integration. Listed as an extension point; the
  Prometheus spec leaves a documented slot.

**Controller metrics** (bounded cardinality, `app` label allowed only because
service count is operator-bounded): reconcile duration histogram,
reconcile/deploy totals by `result`, drift gauge per service, last-successful-
reconcile timestamp, webhook deliveries by `event,result`, git poll
duration/result, SQLite op latency, backup last-success timestamp + verify
result, build_info gauge.

**Independence properties (acceptance-tested in M4):**

1. Prometheus/Grafana/exporters are plain Dokku apps with `restartPolicy:
   always` — Dokku/Docker keep them up whether or not the controller exists.
2. The controller never *requires* Prometheus: it exposes `/metrics` but
   nothing in reconcile/recovery reads monitoring.
3. Recovery/diagnosis (`topctl recover diagnose`) uses only terminal output,
   Dokku CLI, and the filesystem — never Grafana.
4. Dependency direction: `observability` reconciles *after* `core`
   (ADR-0004); Grafana's datasource points at Prometheus by container name
   over the Dokku network (verified in ADR-0001 research).

## Alternatives considered

- **VictoriaMetrics instead of Prometheus** — leaner, but the brief names
  Prometheus and ecosystem familiarity wins for an open-source project.
- **Dockprom-style compose stack outside TOP** — violates self-management
  (ADR-0005). Rejected.
- **Loki for logs in v0** — Dokku's log story (`dokku logs`, `logs:failed`)
  plus controller file logs suffices; Loki is an extension point.
- **Alertmanager in v0** — deferred (above).

## Consequences

- First reconciliation renders Prometheus/Grafana config from the specs'
  mounted files — config changes are normal GitOps changes.
- TOP availability monitoring when TOP is down is out of band by design
  (the host still exists; docs recommend external uptime checks as a user
  choice).
- The Grafana admin password is the first real consumer of `secretRefs`
  (ADR-0008).

## Open questions

- Exact small-host Prometheus settings (retention/size) — tune during M4
  against the documented minimum host profile.
