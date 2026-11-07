# 0006 — Go module and package architecture

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

The brief fixes Go and a monorepo "with each independently meaningful
component in its own directory". We need a package structure that keeps
domain decisions separate from CLI, persistence, Git, and Dokku execution —
without enterprise-java ceremony.

## Decision

**One Go module, one binary.** The module `github.com/toledompm/top` (module
path confirmed with owner) produces a single `topctl` binary; the controller is
`topctl controller run`, the installer is `topctl install`, etc. One binary
keeps installation, versioning, and the bootstrap deploy trivial, and the
controller image is built from the same commit as the CLI that deployed it.

**Layered packages under `internal/`**, dependencies pointing inward:

```
cmd/topctl/                 main + subcommand wiring (cobra), thin
internal/
  domain/                pure types: Service, DesiredState, ObservedState,
                         Plan, Operation, OperationID, states; no I/O
  spec/                  ServiceSpec parse + strict validate (YAML → domain)
  plan/                  desired-vs-observed diff → Plan (pure functions)
  reconcile/             execute plans against PlatformBackend; retries,
                         ordering, operation recording
  backend/               PlatformBackend interface
  backend/dokku/         Dokku adapter (exec-based); the only dokku-aware code
  store/                 StateStore interface
  store/sqlite/          SQLite impl + migrations
  gitsource/             clone/fetch/checkout, revision resolution, validation
  webhook/               HTTP endpoint, HMAC verify, dedup, enqueue
  controller/            loops (poll, reconcile), HTTP server, metrics wiring
  install/               preflight, ownership inventory, bootstrap deploy
  secrets/               host secret store resolution (ADR-0008)
  exec/                  safe subprocess wrapper (timeouts, redaction, argv)
  config/                TOP's own config load/validate/versioning
  obs/                   slog setup, prometheus registry, metrics helpers
```

**Interfaces only at infrastructure boundaries**: `PlatformBackend`,
`StateStore`, `GitSource`. No interfaces for single structs
with one implementation and no test need.

**Contracts live in docs first** (`docs/contracts/`), then become Go
interfaces in Milestone 1. The docs are the review surface; Go signatures
follow them.

## Alternatives considered

- **Multi-module monorepo (one module per component)** — real isolation but
  dependency/version friction for a one-person platform; nothing in v0 needs
  independent release cycles. Rejected for v0; package boundaries are drawn
  so a split is possible later.
- **Separate controller and CLI binaries** — doubles release/artifact and
  bootstrap logic; rejected.
- **Plugin framework for backends** — premature; one backend (Dokku) exists.
  The `PlatformBackend` interface is the extension point, no dynamic loading.

## Rationale and trade-offs

Single binary + internal packages is the smallest structure that enforces
dependency direction (compiler-enforced via `internal/`) and keeps the
e2e/install story one artifact deep. The risk of one module — sloppy imports —
is controlled by the package map above plus `go vet`/lint in CI.

## Consequences

- `internal/backend/dokku` is the *only* package allowed to build dokku
  argv; everything above it speaks `PlatformBackend` (see
  `docs/contracts/platform-backend.md`).
- All host mutation goes through `internal/exec`, which enforces timeouts,
  no shell string interpolation (argv slices only), and secret redaction.
- Domain + plan packages are pure and get high unit-test coverage targets.

## Amendment (M2, 2026-10-27): the bootstrap carve-out

`internal/backend/dokku` remains the only place that *builds Dokku argv*
in normal operation, with one documented exception: the install/bootstrap
path (`internal/install` phase 2, pre-backend bootstrap — ADR-0005:
"small, deterministic, separate from the reconciliation loop"), which
execs the `dokku` CLI directly through the shared `internal/exec` seam;
the exception is structural because the backend assumes an operating TOP.
`internal/inventory` reads dokku/docker state directly for the same
bootstrap reason. Argv command facts may live in two packages by design —
any change to phase-2 verbs must mirror the adapter's command conventions
and document itself in the package READMEs.

## Open questions

- None significant; package list was confirmed against contracts in the
  Milestone 1 review; the M2 carve-out above is the layout's first real
  amendment.
