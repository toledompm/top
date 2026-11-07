# Milestones

Dependency-ordered. Each milestone is independently reviewable, shippable,
and leaves the repo in a coherent state. Details in per-milestone files.

```
M0 Architecture & plan ──► M1 Foundation ──► M2 Bootstrap & host lifecycle
        (this)                  │                     │
                                └──────────┬──────────┘
                                           ▼
                                M3 GitOps controller & workloads
                                           │
                                           ▼
                                M4 Self-managed core & observability
                                           │
                                           ▼
                                M5 Operational hardening  ──► v0
                                           
                                (post-v0: M6 MCP server — ADR-0011)
```

| # | Title | Core deliverable | Key acceptance |
| --- | --- | --- | --- |
| [M0](m0-architecture.md) | Architecture & implementation plan | ADRs, contracts, backlog | owner approval |
| [M1](m1-foundation.md) | Foundation | module, contracts as code, CLI skeleton, sqlite, logging, test seams | unit+shim green; contracts compile |
| [M2](m2-bootstrap-host-lifecycle.md) | Bootstrap & host lifecycle | doctor/install/uninstall, ledger, Dokku adapter (app CRUD) | install/uninstall on clean VM; foreign resources untouched |
| [M3](m3-gitops-controller.md) | GitOps controller | GitSource, webhooks, spec→plan→apply, diff, history | change in Git lands on host; invalid spec rejected safely |
| [M4](m4-self-managed-observability.md) | Self-managed core & observability | install script hands off to GitOps; prometheus/grafana as specs | webhook e2e; poll repairs missed webhook; TOP-down survival |
| [M5](m5-hardening.md) | Operational hardening | failure injection, docs completion, release engineering | all 10 acceptance tests scripted & green |

## Global rules

- No milestone starts before the previous milestone's Definition of Done is
  met (parallel work allowed only where the backlog marks it).
- Every milestone ends with: docs updated, ADRs amended if reality diverged,
  backlog updated, hygiene check (ADR-0014) re-run.
- Host-mutating tests follow the tier rules in
  [`../architecture/testing-strategy.md`](../architecture/testing-strategy.md).

## The 12 v0 acceptance tests

Listed and tier-mapped in
[`../architecture/testing-strategy.md`](../architecture/testing-strategy.md#the-12-acceptance-tests-mapped)
— M4 and M5 are where they become executable.
