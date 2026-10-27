# internal/domain

TOP's **shared vocabulary package**: concrete types and enums that every
other package (spec parser, reconciler, backend, store, CLI) speaks. It is
the semantic model of TOP's desired and observed world; nothing here talks
to disk, the network, Dokku, or SQLite.

## Purpose

- **Identity** — `Namespace` (reserved: `core`, `observability`; default:
  `default`), `Name`, and the qualified `ServiceRef` ("ns/name"), which maps
  1:1 onto the Dokku app via `AppName(ns, name)` → `"<ns>-<name>"`
  (≤48 chars, ADR-0004).
- **Desired state** — `DesiredState` = a resolved Git revision (never a
  branch, ADR-0002) plus the ordered set of resolved `Service`s (image or
  build source, ports, routing, healthcheck, storage, env + secretRefs,
  dependsOn, rollout, restart policy, resources). YAML parsing/defaulting is
  `internal/spec` (T-103); these types are what a spec *means* after parsing.
- **Observed state** — `ObservedState`, what the `PlatformBackend.Inspect`
  contract returns (deployed/running/revision/image, env keys, ports,
  domains, storage, restart policy, resources, networks, build status).
- **Plan** — `Plan`/`ServicePlan`/`FieldChange`: the per-service diff
  (`create|update|delete|noop|drifted-self`) with field-level changes, whose
  rendering is **secret-safe by construction** (see below). `topctl diff` is
  the plan computation with zero side effects (ADR-0002).
- **Operations** — `Operation`: the append-only record of every mutating
  action with uuid ID, kind, actor, correlation ID, the
  `pending → running → succeeded|failed|aborted` lifecycle enforced by
  `Operation.Next`, and a redacted error string.
- **Run summaries & checkpoints** — `ReconciliationRun`, `ServiceOutcome`,
  and `Checkpoint` (lastApplied sha + per-service last-known-good).
- **Status enums** — `ServiceStatus`, `BackendStatus` shared by CLI and
  metrics.
- **Error taxonomy** — sentinels (`ErrNotFound`, `ErrConflict`, `ErrLocked`,
  `ErrCorrupt`, `ErrInvalid`, `ErrUnhealthy`, `ErrBackend`) plus typed
  errors that `Unwrap()` to a sentinel so callers can branch with
  `errors.Is`/`errors.As` while rendering "…; hint: …" messages.
- **Validation helpers** — `ValidNamespace`, `ValidName`,
  `ValidRevision`, `ServiceRef.Validate`, plus `Parse*` constructors for
  every enum.

## Boundaries

- No I/O imports of any kind (no filesystem, network, exec, sql, yaml).
  Everything here is stdlib + `github.com/google/uuid` only.
- **No interfaces** — this is the one deliberately non-abstract package;
  interfaces live at infrastructure boundaries elsewhere.
- **Never carries secret values** — secrets exist only as reference names
  (`SecretRefs`, `SecretChecksums`, secret-safe `FieldChange`). See ADR-0008.
- Pure value types; no global state, no package-level mutable anything.

## Conventions

- Enum types: string-backed, `IsValid()` method, `String()`, and `Parse<X>`
  returning `(T, error)`; zero value of `RestartPolicy`/modes means
  "unspecified — spec defaults live in internal/spec".
- Pointer/setter presence beats booleans where absence is meaningful
  (`HealthCheck` nil = Dokku defaults; `FieldChange.From/To` nil = absent).
- Table-driven tests cover every enum transition, `AppName` validation,
  `ServiceRef` round-trips, the operation state machine, and the
  secret-safe plan rendering proof.
