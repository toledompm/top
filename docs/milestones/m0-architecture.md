# Milestone 0 — Architecture and implementation plan

## Objective & scope

Research the environment (repo, memory, Dokku docs, Go ecosystem, test host),
produce ADRs, contracts, milestone plans, backlog, risk register, and test
strategy. **No implementation.**

## Non-goals

- Any production code, host changes, or deployments.
- Committing/pushing anything (awaiting owner approval).

## Deliverables

- 14 ADRs (`docs/adr/`), 8 contracts (`docs/contracts/`), architecture
  overview + risk register + test strategy (`docs/architecture/`),
  milestone plans, authoritative backlog (`docs/tasks/backlog.md`),
  operations runbook skeletons (`docs/operations/`), repo hygiene files
  (`.gitignore`, `AGENTS.md`, `examples/`).

## Acceptance criteria

- [ ] Owner review: every "needs explicit approval" ADR (0001, 0002, 0004,
      0005, 0008, 0009, 0011, 0012) is accepted, amended, or rejected.
- [ ] Unresolved questions list is acknowledged (end of `docs/tasks/backlog.md`).
- [ ] Hygiene review passes: no private infrastructure details in tracked files.

## Verification state (this milestone)

- Dokku capability claims: verified against official docs (see overview's
  assumptions register).
- Library currency: verified 2026-10-27.
- Test-host inspection: read-only, findings kept local per ADR-0014; only
  generalized requirements entered ADRs (min resources, no-passwordless-sudo
  handling, Dokku-absent install path, port-conflict avoidance).

## Risks

- Planning on docs that drift → mitigated by min-version pinning (ADR-0001)
  and tier-3 CI in later milestones.

## Definition of done

Owner explicitly approves the plan and green-lights Milestone 1.
