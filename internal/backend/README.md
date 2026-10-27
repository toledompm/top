# internal/backend

The **PlatformBackend interface** (docs/contracts/platform-backend.md): TOP's
runtime abstraction over the workload engine. This package defines only the
interface; the single v0 implementation lives in `dokku/` and is the only
place in the tree allowed to know Dokku command syntax (ADR-0001).

## API

```go
type PlatformBackend interface {
    EnsureApp(ctx, ref domain.ServiceRef) error
    Inspect(ctx, ref domain.ServiceRef) (domain.ObservedState, error)
    LastGoodRevision(ctx, ref domain.ServiceRef) (string, error)
    Stop(ctx, ref domain.ServiceRef) error
    Destroy(ctx, ref domain.ServiceRef) error
    HealthyBackend(ctx) (bool, error)
    EnsureNetwork(ctx, name string) error
    Close() error
}
```

Every method takes a context (cancellation always honored), maps host
reality into `internal/domain` types, and is reconciler-consumable (M3).
Refs are qualified team identities ("ns/name"); the Dokku app name is
derived by `domain.AppName` as `"<ns>-<name>"`.

## Interface-vs-contract deviations (v0 slice, T-201/T-202)

- **`Apply`, `Readiness`, `FailedLogs` are NOT on the interface yet** —
  they arrive with M3 (T-305..T-308) and extend, never fork, this
  interface. The contract's apply-time mapping is documented in the
  contract; nothing here implements it yet.
- **`Inspect` domains**: the contract maps domains, but no domains-flag
  was verified this wave — `ObservedState.Domains` is left unset (see
  `dokku/README.md`); an M3 follow-up extends the composition.
- **`EnsureApp` adoption**: adopt-if-exists follows ADR-0009's *naming*
  layer only; the ledger two-key rule is the caller's (T-204/T-402), so a
  knowingly-foreign app is resolved upstream, not in `EnsureApp`. `Destroy`
  is likewise caller-gated — the adapter's role is documented in
  `dokku/README.md` (ownership split).

## Testing tiers (ADR-0012)

- Tier 1 (default `make test`): fake-exec golden argv — `dokku/`'s table.
- Tier 3 (`make test-e2e-backend`, `TOP_E2E=1`): real Dokku in a
  disposable container — `dokku/e2etest/`.
- Tier 2 (shim) is `test/shim`'s; this package installs nothing there yet.

Sub-package boundaries: `dokku/` must never be imported by anything above
the reconciler except through THIS interface.
