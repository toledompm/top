# internal/ — package index

TOP is one Go module (`github.com/toledompm/top`, ADR-0006) producing one
binary, `topctl`. Everything below hides behind `internal/`; dependencies
point inward (pure packages on top, I/O at the bottom). Each package ships
its own README with its exported API and invariants — start there.

## Present (Milestone 1)

| Package | Role | Contract / ADR |
| --- | --- | --- |
| `domain/` | Shared value types: Service/ObservedState/Plan/Operation, enums, error taxonomy. Pure, no I/O, no interfaces. | contracts/README.md "Shared semantics" |
| `spec/` | ServiceSpec YAML parse + strict validation (`top/v1alpha1`), defaults, `file:line` errors, warnings. | service-spec.md, ADR-0004 |
| `plan/` | Pure diff engine: `(desired, observed) → Plan`. Idempotency invariant. Secret-safe by construction. | reconciler.md, ADR-0002/0005 |
| `exec/` (+ `exec/exectest`) | Safe subprocess wrapper: argv-only, ctx timeouts, env hygiene, redaction, typed exit errors. | ADR-0006, ADR-0008/0009 redaction |
| `store/` (interface) | The StateStore surface callers depend on. | state-store.md, ADR-0007 |
| `store/sqlite/` | SQLite impl + goose embedded migrations (schema v1), lease, ledger, delivery dedup, downgrade protection. | state-store.md, ADR-0007 |
| `secrets/` | `SecretProvider` seam; v0 `hostfile` impl (0600/0700, checksum versions); `vault` reserved at `ErrUnimplemented`. | ADR-0008 |
| `config/` | TOP's own strict versioned config (`top/v1alpha1`), `TOP_*` env overlay, typed validation. | ADR-0006 |
| `obs/` | slog JSON + correlation, Prometheus metrics vs ADR-0010's run (private registry). | ADR-0010 |
| `cli/` | The full `topctl` command tree as validated stubs: flags, envelopes, exit codes, confirm gates. | cli.md, ADR-0009/0011 |

## Not present yet (by plan)

| Package | Lands | Contract |
| --- | --- | --- |
| `backend/` + `backend/dokku/` | M2 (read slice) + M3 (Apply/Readiness) | platform-backend.md, ADR-0001 |
| `install/` | M2 | installer.md, ADR-0005/0009 |
| `recover/`, `backup/` | M5 | backup-recovery.md, ADR-0005/0007 |
| `gitsource/`, `webhook/`, `controller/`, `reconcile/` | M3 (loops wired M3/M4) | git-source.md, reconciler.md, ADR-0003/0010 |

Outside `internal/`: `cmd/topctl` (thin main), `conformance_check/`
(T-111 contract-conformance harness — meta-package, never imported by
production code), and `test/` (tier-2 seams, fakeexec, dokku shim, plus the
hygiene scanner — see `docs/architecture/testing-strategy.md`, ADR-0012).
