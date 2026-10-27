# Milestone 1 — Foundation

## Objective & scope

Stand up the Go module and engineering skeleton that everything else builds
on: contracts-as-code (interfaces + domain types), config, logging, exec
layer, SQLite store with migrations, CLI skeleton, and test seams (tiers 1–2
from ADR-0012).

## Non-goals

- Real Dokku calls (the adapter gets its interface + fake; real calls are M2/M3).
- Controller loops, webhooks, metrics endpoint (M3/M4).

## Prerequisites

- M0 approved. Go toolchain (≥ 1.26, per client_golang v1.25 requirement).

## Deliverables

- `cmd/topctl` with cobra commands as no-op/validated stubs; `-o json` plumbing
  with `schemaVersion`.
- `internal/domain`, `internal/spec` (strict YAML via `go.yaml.in/yaml/v3`,
  `KnownFields`), `internal/plan` (pure diff).
- `internal/exec` (`Runner` interface: argv-only, timeouts, WaitDelay, env
  hygiene, redaction) + `test/fakeexec` + dokku shim.
- `internal/store/sqlite` (modernc driver, DSN pragmas, goose migrations:
  schema v1 = operations/checkpoints/deliveries/ledger/backups/leases).
- `internal/config`, `internal/obs` (slog JSON + level var; registry helper).
- CI: tiers 1–2, lint/vet/fmt, ADR-0014 hygiene grep.
- Package READMEs; updated AGENTS.md if conventions changed.

## Acceptance criteria

- [ ] `go build ./...`, `go test ./...` green on Linux+macOS from a clean clone.
- [ ] ServiceSpec contract examples parse; invalid samples fail with file:line.
- [ ] Store round-trips an Operation through migrations from zero.
- [ ] Redaction unit tests prove registered secrets never reach captured output.
- [ ] CI passes on a fork with no secrets.

## Failure scenarios to test

- Corrupt YAML, unknown fields, both image+build, bad names.
- SQLite busy (two writers) → busy_timeout path.
- exec: timeout kill, non-zero exit, stderr-on-success, redaction.

## Documentation

- `docs/contracts/*` updated to match code realities; each package README.

## Risks & rollback

- Contract drift between docs and code → contracts compile-checked in tests.
  Rollback: none needed (no host impact).

## Definition of done

All criteria green in CI; backlog tasks M1-* closed; owner tag review.
