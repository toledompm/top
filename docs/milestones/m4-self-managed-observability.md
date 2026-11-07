# Milestone 4 — Self-managed core and observability

## Objective & scope

Close the bootstrap loop: the example platform repo (a fictional public twin
lives in `examples/`) defines `core` (the controller itself)
and `observability` (Prometheus, Grafana, node_exporter, cAdvisor) as
ServiceSpecs; `topctl install` hands management of them to the reconciler.
Prove independence: TOP down ⇒ workloads + monitoring keep running.

## Non-goals

- Alertmanager/Loki (extension points, ADR-0010).
- Automatic controller self-upgrades (human-gated per ADR-0005).

## Prerequisites

- M3 done.

## Deliverables

- `examples/platform-repo/`: publishable, fictional ServiceSpecs for
  `core/top-controller`, `observability/*`, with provisioned Grafana
  dashboards/datasources and small-host Prometheus defaults.
- Installer phase 5 (first reconciliation adopts bootstrap deployment;
  ledger conversion `bootstrap-owned → managed`).
- Observability wiring: shared `top-internal` network attachments, scrape
  config rendered from specs, Grafana admin via `secretRefs`.
- Controller metrics dashboard JSON (provisioned).
- E2E scripts for acceptance tests 2, 3, 4, 8.

## Acceptance criteria (maps to global tests 2, 3, 4, 8)

- [ ] Install per public docs on a clean VM → `core` + `observability`
      reconciled green; Grafana reachable per its routing mode.
- [ ] Git change to an observability spec → webhook → converged (and the same
      via poll with the webhook deliberately dropped).
- [ ] `docker kill` the controller (and later `dokku ps:stop`): Prometheus,
      Grafana, exporters, and a sample user app keep running; controller
      returns via Dokku restart policy; missed changes reconcile on return.
- [ ] `topctl diff` shows drift on `core-top-controller` when its spec
      revision advances, and `topctl deploy core/top-controller` applies it
      (human-gated path) with old-release preservation on failure.

## Failure scenarios

- Grafana down ≠ blindness: `topctl doctor` still fully functional.
- Prometheus spec broken → reconcile fails contained; controller unaffected.
- Controller self-deploy to a broken image → checks fail, old controller
  keeps running, op recorded failed.

## Documentation

- `docs/operations/observability.md` (what's scraped, dashboards, sizing);
  README quick-start verified verbatim on a clean VM.

## Risks & rollback

- Small-host OOM from the stack → resource limits + retention caps tuned
  here (R10); rollback = scale down specs via Git (dogfood proof).

## Definition of done

Acceptance green end-to-end from a clean VM twice in a row; backlog M4-*
closed.
