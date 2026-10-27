# AGENTS.md — TOP (Toledo's Opinionated Platform)

TOP is an opinionated, self-hosted, single-node application platform that brings
GitOps-style continuous reconciliation to Dokku — think "Flux for Dokku".

**Status: Milestone 0 (architecture & planning). No implementation exists yet.
Do not write production code until the owner explicitly approves the plan in
`docs/adr/` + `docs/tasks/backlog.md`.**

## Repository layout

```
docs/architecture/   system overview, risk register, test strategy
docs/adr/            architecture decision records (product decisions, portable)
docs/contracts/      ServiceSpec, PlatformBackend, StateStore, etc.
docs/milestones/     milestone plans with acceptance criteria
docs/operations/     install / recovery / backup / teardown runbooks
docs/tasks/          authoritative implementation backlog (backlog.md)
examples/            fictional, committable example configs
.local/              PRIVATE local notes (gitignored — never commit)
```

## Hard rules for agents

1. **Public/private boundary is sacred.** Never commit real hostnames, SSH
   targets, tailnet addresses, IPs of real machines, credentials, tokens, or
   local diagnostic output. Committed examples use fictional values
   (`apps.example.com`, `github.com/example/...`, `deploy@example-host`).
   Machine-specific material lives only in `.local/` (gitignored).
2. **No host mutations without explicit approval.** Read-only inspection of the
   author's test host is fine when authorized; installing packages, touching
   docker resources, firewall, or Dokku requires an explicit go-ahead for that
   specific operation.
3. **Keep the plan in tracked docs.** Durable decisions live in ADRs, not in
   chat. If a decision changes, amend the ADR (status: superseded) rather than
   contradicting it silently.
4. **One authoritative backlog.** `docs/tasks/backlog.md` is the only task
   list; new work is appended there, never in side files.
5. **Go conventions (for Milestone 1+).** `go vet` + `gofmt` + `golangci-lint`
   clean; small packages under `internal/`; slog for logging; errors wrapped
   with context; every component ships a README.
6. **Testing.** Unit tests are portable and hermetic. Anything needing a real
   Dokku host is gated behind an env flag (see docs/architecture/testing-strategy.md)
   and must default to a disposable environment, never the author's server.

## Conventions

- **License**: MIT (`LICENSE`). Go source files carry an SPDX header:
  `// SPDX-License-Identifier: MIT` in the file's leading comment block.
  Copyright holder in `LICENSE` is "toledompm and TOP contributors".
- **Bend laws** (`laws/LAWS.bend` + `PROOF.bend`): machine-checked invariants
  over TOP's pure core. When using Bend: run `bend guide` to learn it; keep
  important rules as laws in `LAWS.bend` (human-owned claims — the AI
  maintains `PROOF.bend` and the code); run `make laws` (proof gate +
  differential bridge) before committing any change to `laws/`,
  `internal/domain`, or `internal/plan`. SCOPE GUARD: laws cover the pure
  core only — exec, store, and dokku-adapter behavior stays in the Go test
  tiers; if the Bend reference model changes, `test/bridge` must stay green.
  See `laws/README.md` + `docs/adr/0015-bend-laws.md`.
- ADR format: see `docs/adr/README.md`.
- Naming: TOP-managed things are prefixed `top-` (apps, volumes, files under
  `/var/lib/top`); namespaces are logical, see ADR-0004.
- Docs are written for a competent engineer who has never seen the author's
  infrastructure. Define terms on first use. Prefer short docs over wikis.
