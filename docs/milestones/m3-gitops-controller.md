# Milestone 3 — GitOps controller and workload deployment

> ## M3 review (T-311, 2026-10-31, pre-owner sign-off)
>
> **Fleet result**: T-301/T-303/T-304 + T-305..T-307 (Wave A, `8a4f4a0`),
> T-302/T-308/T-309/T-212 (Wave B, `8b1faa3`), docs/ADR pass + acceptance
> verification (Wave C, `b8fe936`, `72a947b`). All M3 backlog tasks
> delivered except T-311 sign-off itself.
>
> **Gate matrix**: vet OK · 20 packages race-clean · conformance OK ·
> shim OK · laws green · gofumpt clean · lint 0 issues · hygiene pass ·
> **tier-3 e2e 8/8 PASS on real Dokku 0.38.32** (disposable container:
> FullRange apply, failing-check old-release preservation, git-sync
> build, HealthyBackend, EnsureNetwork, Ensure-and-never-deployed,
> AppLifecycle, and the new **reconcile-pipeline-lands-spec-change**
> proof through the real dokku app).
>
> **Acceptance criteria (milestone §) — status**:
> 1. spec change lands on a real Dokku app — PASS (tier-3
>    `TestE2EReconcilePipelineLandsSpecChange`; tier-4 VM run deferred
>    to M4 soak per plan).
> 2. signed webhook → 202 → reconcile; forged → 401 no enqueue; dup →
>    single run — PASS (internal/webhook table tests + controller
>    coalescing proof `TestQueueTriggerStormCoalescesToSinglePending`,
>    50 concurrent → 1 pending).
> 3. `diff` exit 1 with drift → after reconcile diff empty — PASS at
>    fake tier (`TestReconcileDriftThenEmptyDiff`, the CI-gate
>    candidate); the *real*-dokku variant of the same invariant is
>    covered indirectly by FullRange's zero-mutation re-apply
>    assertion (zero mutations AND zero archive resends).
> 4. invalid spec → pre-mutation fail with file:line — PASS
>    (validateRun tests, no-lease/no-record verified).
> 5. broken image → op failed, old release serving, actionable
>    history/logs — PASS (tier-3
>    `TestE2EApplyFailingCheckPreservesOldRelease` ×  + logs/hint tests).
> 6. mutable-tag move detected by poll → redeployed — PARTIAL: the
>    digest/tag `--force` path and poll sha-change detection are coded
>    and unit-tested; the full tier-3 tag-move proof is the natural
>    first test of the M4 soak (requires a running daemon against a
>    live repo — the harness contract for it is documented in
>    internal/controller README).
>
> **Amendments applied (docs committed)**: ADR-0002 (adapter-owned
> `TOP_ARCHIVE_SHA256` carve-out; K8s→docker quantity translation;
> two refinements: zero-mutation skip requires Running, `builder:set
> skip-cleanup` recorded as NOT implemented gap); ADR-0003 (webhook
> real: header set, 64 KiB cap, duplicate→200 envelope, dedup-only-
> on-parse, per-request secret rotation, no-TTL — resolves the ADR's
> "decide in M3" question); ADR-0009 (prune consent pair fail-fast
> before lease). Contracts updated to code truth: platform-backend
> (incl. ServiceLogs row, deploy timeout 10m), cli (real bodies +
> lease-conflict exit 3), git-source, installer (--init), reconciler
> (Diff-only path). state-store verified no-drift.
>
> **CI wiring proposals (for owner)**: (a) the diff-empty invariant
> (`TestReconcileDriftThenEmptyDiff`) as the idempotency CI gate
> required by the milestone DoD; (b) conformance harness joins PR CI
> already satisfied — verify `ci.yml` covers conformance_check with
> the doc-lint budget; (c) `make laws` job enforced (currently
> optional) — decision deferred to owner per ADR-0015.
>
> **Owner signs off on**: amendments above, CI proposals, M4 name
> (self-management + soak + first-reconcile adoption).


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
