# Platform repo — initial template content

This repository is TOP's GitOps source of truth (ADR-0002): every TOP-managed
workload is a ServiceSpec file at `<namespace>/<name>.yaml`
([docs/contracts/service-spec.md](../../../docs/contracts/service-spec.md)) and
the controller converges the host to the specs at the pinned revision.
This exact content is embedded in `topctl` and committed by
`topctl install --init` when the configured platform repo resolves empty
([ADR-0005](../../../docs/adr/0005-bootstrap-self-management-recovery.md)).

## Layout (v0 template)

- `core/top-controller.yaml` — TOP's controller, self-managed like any
  workload. Self-updates are human-gated (ADR-0005): apply new revisions with
  `topctl deploy core/top-controller` — never automatically.
- `observability/prometheus.yaml` — commented, ready-to-enable example
  (prometheus-ready v0 shape). Uncomment only after pinning image digests
  you actually want to run; everything shipped here is fictional.

## Placeholders

Lines marked `{{PLACEHOLDER}}` are rendered by `topctl install --init` at
seed time (existential placeholders only — TOP never bakes secrets into
git). All other values are the fictional documentation discipline
(`git.example.com`, `apps.example.com`) and are safe to commit
([ADR-0014](../../../docs/adr/0014-public-local-boundary.md) governs the TOP
source repo; keep this template fictional as long as it lives there).

## Workflow

```
git edit <namespace>/<name>.yaml  →  webhook or 60 s poll triggers a run
topctl diff                        →  the plan, before anything applies
topctl reconcile --now             →  converge immediately (or wait for poll)
```

Deleted spec files are orphaned; removal always needs the explicit
`topctl reconcile --prune` (never for `core` — ADR-0004).
