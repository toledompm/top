# Milestone 3 — GitOps controller and workload deployment

## Objective & scope

The heart: GitSource (clone/poll/webhook), spec→plan→apply reconciliation,
`topctl diff`, `topctl history`, readiness gating, retries, drift reporting and
correction, operation records, and the controller's HTTP server (webhook,
healthz, metrics).

## Non-goals

- Self-management wiring (M4 turns the loop onto TOP itself).
- Backup scheduling (M5). Fine-grained per-path webhook fast paths (never?).

## Prerequisites

- M2 done (adapter CRUD, ledger, install).

## Deliverables

- `internal/gitsource`: git-CLI fetch/archive, deploy-key auth, ls-remote
  polling, revision resolution (ADR-0008, git-source contract).
- `internal/webhook`: HMAC verify, delivery dedup, 202+enqueue (ADR-0003).
- `internal/reconcile`: full pipeline per reconciler contract; lease;
  ordering; readiness gates; last-known-good tracking; prune-gated deletes;
  controller-self special case (report-only).
- `internal/backend/dokku` completion: `Apply` (image + build sources,
  batched config, storage, networks, ports/domains, resources, checks),
  `Readiness`, `LastGoodRevision`, `FailedLogs`.
- `topctl diff|reconcile|deploy|history|logs` wired end-to-end.
- `internal/controller`: loops, queue coalescing, HTTP server, metrics
  (ADR-0010 set).

## Acceptance criteria (maps to global tests 3, 5, 6)

- [ ] Push to repo (or manual `reconcile`) lands a spec change on a real
      Dokku app (tier 3 + scripted tier 4).
- [ ] Signed webhook → 202 → reconcile; forged signature → 401 + no enqueue;
      duplicate delivery → no second run.
- [ ] `topctl diff` exit 1 with exact field-level drift; after reconcile, diff
      is empty (idempotency invariant).
- [ ] Invalid spec → run fails pre-mutation, nothing changes, error names
      file:line.
- [ ] Broken image (failing checks) → op `failed`, old release still serving,
      `topctl history` shows actionable error.
- [ ] Mutable-tag move is detected by poll and redeployed (`--force` path).

## Failure scenarios

- Git outage mid-run → abort before mutation.
- Webhook storm → coalescing proof.
- Force-push (sha gone) → loud failure, no mutation.
- Concurrent CLI reconcile during controller run → lease serializes.

## Documentation

- contracts updated to code truth; `docs/operations/` gains reconcile/diff
  how-to; ADR-0002 amended if any mapping changed.

## Risks & rollback

- Synthesized-archive healthcheck path misbehaves → fallback: Dokku default
  checks + loud warning (flagged in backend contract; decide by test result).
- Queue/lease bugs → chaos tests in tier 3; rollback = previous milestone tag.

## Definition of done

Acceptance green; idempotency invariant test in CI; backlog M3-* closed.
