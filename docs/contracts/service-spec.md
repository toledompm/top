# Contract: ServiceSpec

The declarative desired-state definition of one workload. Lives in the
platform Git repo at `<namespace>/<name>.yaml`. The Reconciler converges the
host to the set of ServiceSpecs at the pinned revision. See ADR-0004 for
semantics and rationale.

## Rules

- **Strict parsing**: unknown fields are a validation error (typos must fail
  loudly). `apiVersion`/`kind` are required and versioned.
- **Immutable identity**: `metadata.name` + `metadata.namespace` map 1:1 to
  the Dokku app `<namespace>-<name>`; renaming = destroy + create (drift-prune
  rules apply).
- **Immutable revisions**: `source` must resolve to an unambiguous revision —
  an image tag (digest recommended) or a git SHA. Mutable references
  (`latest`, branch names) are allowed but produce a validation *warning* in
  `topctl diff` output.
- **No secret values**: `env` holds plain values only; anything sensitive is
  a `secretRefs` entry resolved from the host secret store (ADR-0008).
- **Defaults are explicit in docs**: omitted fields take the defaults below
  and `topctl diff` shows the effective (defaulted) spec.

## Field summary (v0)

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `apiVersion` | string | — | `top/v1alpha1` only |
| `kind` | string | — | `Service` only |
| `metadata.name` | string | — | `[a-z0-9-]`, ≤48 chars |
| `metadata.namespace` | string | `default` | `core`/`observability` reserved |
| `spec.source.image` | string | — | registry path (image deploy) |
| `spec.source.tag` / `digest` | string | — | exactly one |
| `spec.source.build.repo` / `ref` / `path` | string | — | alternative to image (Dokku source build) |
| `spec.ports[]` | list | `[]` | `name`, `containerPort`, `hostPort?` (discouraged) |
| `spec.routing.mode` | enum | `none` | `proxy` (dokku nginx + domains) \| `internal` (container network only) \| `none` |
| `spec.routing.domains[]` | list | `[]` | required when `proxy` |
| `spec.healthcheck.http/tcp/exec` | object | — | maps to Dokku checks; absence = Dokku defaults |
| `spec.storage[]` | list | `[]` | `name`, `mountPath`, `size` (advisory) → dokku storage |
| `spec.env` | map | `{}` | plain values |
| `spec.secretRefs[]` | list | `[]` | names resolved via the configured `SecretProvider` (host-file store in v0; Vault-style providers later — ADR-0008) → injected as env |
| `spec.dependsOn[]` | list | `[]` | intra-namespace ordering hints |
| `spec.rollout.maxUnavailable` | int | `0` | `0` = require zero-downtime checks to pass |
| `spec.restartPolicy` | enum | `always` | maps to dokku ps:restart-policy |
| `spec.resources.limits.cpu/memory` | string | — | optional; maps to dokku resource plugin |

## Illustrative example (fictional, from `top-homelab`-style repo)

`observability/prometheus.yaml`:

```yaml
apiVersion: top/v1alpha1
kind: Service
metadata:
  name: prometheus
  namespace: observability
spec:
  source:
    image: docker.io/prom/prometheus
    digest: sha256:0123456789abcdef…   # fictional
  ports:
    - { name: http, containerPort: 9090 }
  routing:
    mode: internal                      # reachable by grafana over the docker network
  healthcheck:
    http: { path: /-/healthy, port: http }
    intervalSeconds: 30
  storage:
    - { name: data, mountPath: /prometheus, size: 2Gi }
  env:
    TZ: UTC
  restartPolicy: always
```

`core/top-controller.yaml` exists in the same repo — TOP's own spec is just a
Service (with `metadata.annotations.top/special: bootstrap` marking the
human-gated self-update from ADR-0005).

## Validation errors (examples)

- `spec.source` sets both `image` and `build` → error.
- `routing.mode: proxy` with empty `domains` → error.
- `secretRefs` entry not resolvable by the configured secret provider →
  error at apply time (warning at diff time, since the provider is
  authoritative).
- Namespace `core` used by a repo not flagged as the platform repo → error
  (protects against tenant repos claiming platform namespaces).

## Lifecycle

Created/updated via Git merge → next reconciliation (webhook-triggered,
polled, or manual). Deleted from repo → orphaned; removed only via explicit
`topctl reconcile --prune` (never for `core`, ADR-0004).
