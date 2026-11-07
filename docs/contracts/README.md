# TOP contracts

Contracts are the review surface between architecture and implementation.
Each document defines responsibilities, inputs/outputs, error semantics, and
lifecycle behavior. Final Go interfaces are derived from these in Milestone 1 —
the docs are the source of truth until code exists, after which code +
generated docs take over and these remain the rationale.

| Contract | Purpose | Doc |
| --- | --- | --- |
| ServiceSpec | Declarative desired-state workload definition (lives in Git) | [service-spec.md](service-spec.md) |
| PlatformBackend | Runtime abstraction over Dokku (inspect/apply/ready/report/stop) | [platform-backend.md](platform-backend.md) |
| StateStore | SQLite persistence for state, operations, checkpoints | [state-store.md](state-store.md) |
| Reconciler | Diff → plan → apply loop, retries, idempotency, serialization | [reconciler.md](reconciler.md) |
| GitSource | Repo registration, revision tracking, webhook/poll intake | [git-source.md](git-source.md) |
| Installer/Bootstrap | Preflight, ownership inventory, bootstrap deploy, teardown | [installer.md](installer.md) |
| CLI | Command surface, JSON output schema, safety flags | [cli.md](cli.md) |

## Shared semantics

- **Operation tracking**: every mutating action is an *Operation* with a
  stable `OperationID`, `kind`, `state` (`pending` → `running` →
  `succeeded` | `failed` | `aborted`), actor (`user`, `controller`,
  `webhook`, `schedule`), `correlationID`, started/finished timestamps, and a
  redacted error string on failure. Operations are append-only.
- **Idempotency**: applying the same desired state twice changes nothing and
  returns the same plan (empty diff). Retried operations resume from recorded
  checkpoints, never from scratch.
- **Errors**: typed, wrapped with context (`fmt.Errorf("...: %w")`), and
  surfaced with an actionable hint ("dokku reported X; check `dokku ps`.
 Graceful: if a hint names a TOP verb, that verb must exist on the CLI surface.").
- **Redaction**: no contract ever carries a secret value; only secret
  *references* (names). See ADR-0008.
