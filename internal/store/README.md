# internal/store — StateStore contract

Persistence contract TOP's controller and CLIs are written against
(`docs/contracts/state-store.md`; decisions in ADR-0007). One implementation
exists in v0: [`sqlite/`](sqlite/README.md) — SQLite via `modernc.org/sqlite`
with the ADR-0007 pragma policy, goose-embedded migrations, and the
single-writer connection policy.

## Teaching points for contributors

- **Everything above this interface is backend-agnostic.** Domain/plan/CLI
  code must not import `internal/store/sqlite` — only this package.
- **Errors are classified**, never raw driver errors: `ErrConflict`,
  `ErrLocked` (with holder identity), `ErrCorrupt` (plus a `Check`/`Open`
  integrity path and schema-downgrade protection via `ErrSchemaNewer`).
- The interface documents the *responsibility list* (operations, observed
  state, checkpoints, delivery dedup, resource ledger, backup registry,
  leases). Adding a method means updating the contract doc in the same PR —
  that's checked by `conformance_check` doc-lint and behavior probes
  (see `conformance_check/README.md`).
