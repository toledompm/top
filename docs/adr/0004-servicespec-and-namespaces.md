# 0004 — ServiceSpec shape and namespace semantics

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes**

## Context

TOP needs a declarative workload format (the desired state in Git) and a
grouping concept (`core`, `observability`, user workloads). The format must be
small enough for one operator to hold in their head, map cleanly onto Dokku
capabilities, and avoid pretending to be Kubernetes.

## Decision

**ServiceSpec**: one YAML file per workload, stored in the platform Git repo
under `<namespace>/<name>.yaml`. Kubernetes-*inspired* field names, but only
fields TOP actually implements. A field is only added when a Milestone
delivers the behavior behind it.

Proposed v0 schema (illustrative, full contract in
[`docs/contracts/service-spec.md`](../contracts/service-spec.md)):

```yaml
apiVersion: top/v1alpha1
kind: Service
metadata:
  name: prometheus
  namespace: observability
spec:
  source:
    image: docker.io/prom/prometheus
    tag: v3.7.3            # deploys pin tag; digest pinning recommended
  ports:
    - name: http
      containerPort: 9090
  routing:
    mode: internal         # internal | proxy | none
  healthcheck:
    http: { path: /-/healthy, port: http }
    intervalSeconds: 30
  storage:
    - name: data
      size: 2Gi
      mountPath: /prometheus
  env:
    TZ: UTC
  secretRefs: []           # names resolved from host secret store, never inline
  rollout:
    maxUnavailable: 0      # rely on Dokku zero-downtime checks
  restartPolicy: always
```

**Namespaces are logical, not structural isolation.** A namespace is:

1. a **name prefix** on the Dokku app: `<namespace>-<name>` (e.g.
   `observability-prometheus`), which gives ownership identification and
   `dokku` CLI ergonomics;
2. a **policy bucket**: reconcile order (`core` first, `observability` second,
   everything else last), and protectedness (drift-pruning disabled for
   `core` in v0);
3. a **documentation/discovery grouping** in status output.

A namespace is explicitly **not**: resource isolation, network isolation,
quotas, or RBAC. Those words are banned from v0 docs in the Kubernetes sense.

## Alternatives considered

- **Full k8s-manifest compatibility** — massive surface for zero v0 value;
  invites wrong expectations. Rejected.
- **One `services.yaml` with a list** — simpler but produces noisy diffs and
  poor per-service history; per-file layout matches Git review workflows.
- **Dokku app = namespace via naming only, no namespace concept in spec** —
  loses ordering and policy grouping that self-management (ADR-0005) needs.
- **Namespaces as separate Git repos** — overkill; one repo, many dirs.

## Rationale and trade-offs

The namespace-as-prefix convention makes ownership detectable with plain
`dokku apps:list` and `docker ps`, which the ownership safeguards (ADR-0009)
and the adoption/teardown paths (ADR-0005) depend on. Ordering by namespace
gives us "platform before tenants" without a dependency engine; `dependsOn`
inside a namespace handles the rare intra-namespace ordering need.

Cost: Dokku app names embed the namespace, so renaming a namespace = recreating
apps. Accepted: namespaces are stable platform vocabulary, not user data.

## Consequences

- Reconciler must topologically order: namespace tier (`core` →
  `observability` → default) then `dependsOn`.
- Drift pruning (deleting apps removed from Git) must never touch `core`
  in v0 and must require explicit `--prune` anywhere else.
- Validation rejects unknown fields (strict YAML) so typos fail loudly.

## Open questions

- Exact app-name prefix format (`observability-prometheus` vs `obs-prometheus`)
  — decide at implementation; contract hides it behind a naming function.
- Do we need per-namespace config defaults (e.g. domain suffix)? Defer until
  a real use case appears.
