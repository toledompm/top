# 0002 — Git as source of truth; the reconciliation model

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes**

## Context

TOP's core loop: desired state in Git, actual state on a Dokku host, and a
controller that converges the latter to the former — idempotently, with
explainable operations and safe handling of partial failure.

## Decision

**Desired state** = the set of ServiceSpecs (ADR-0004) at a resolved Git
revision of the platform repo. The controller keeps a bare clone and resolves
`<branch> → sha`; every reconciliation is pinned to a sha, never a moving
branch name.

**Reconciliation is level-based, not edge-based.** Each run: fetch → resolve
sha → load+validate specs → for each service (ordered by ADR-0004):
inspect actual via `PlatformBackend.Inspect` → diff → apply → verify
readiness → record. Webhooks/polls merely *enqueue* a run; a missed webhook
is repaired by the next poll, and a duplicated one is deduped
(`X-GitHub-Delivery`) or converges to a no-op. Nothing depends on any single
delivery.

**Deploy mapping** (per ServiceSpec source type, to verified Dokku commands):

| ServiceSpec | Dokku mechanism | Why |
| --- | --- | --- |
| `source.image` (+tag/digest) | `git:from-image` (digest preferred; `--force` when a tag may have moved) | idempotent (same args → no-op exit 0), records `source-image`, rolls deploy branch back on failure |
| `source.build` (repo+ref+path) | `git:sync --build-if-changes <app> <repo> <sha>` | Dokku builds (herokuish/Dockerfile); no-change poll leaves no build record |
| custom healthchecks on image deploys | synthesize a tiny archive (Dockerfile `FROM` + `app.json` checks) → `git:from-archive -- tar` via stdin | image deploys have no repo to carry `app.json`; archive path is documented and stdin-supported |

**Env/config**: batch with `config:set --no-restart`, then one explicit
restart/deploy — never N restarts for N vars. Dokku's failure mode (exit
non-zero, old env intact, no restart) is preserved and surfaced.

**Drift correction**: diff covers app existence, revision/image, env keys
(names + plain values; secrets by reference presence), ports/proxy/domains,
storage mounts, restart policy, resource limits, and network attachments.
Correction is *apply desired*; deletion of apps removed from Git requires
explicit `--prune` (ADR-0004).

**Rollback**: re-apply the previous desired revision (recorded as
last-known-good per service). For image services this is a
`git:from-image` of the old digest; Dokku's zero-downtime checks gate the
switch the same as a forward deploy. `builder:set skip-cleanup true` is set
on managed apps so previous images remain available for fast rollback.

**Failure semantics**: a service failing readiness/checks stops its own
operation (`failed`, recorded), does not block independent services, and does
not advance the repo-wide checkpoint (next run retries). The old release
keeps serving because Dokku retires old containers only after successful
checks.

## Alternatives considered

- **Edge-triggered only (webhook-driven apply, Flux 1.x style)** — brief
  already judges this: webhooks are an optimization. Level-based polling is
  the correctness floor.
- **Controller-side build + push to Dokku git remote** — requires controller
  write access pattern and build plumbing that `git:sync` already provides
  on-host. Rejected for v0.
- **Mutable-tag tracking as first-class** — digests are the recommended
  revision; tags allowed with a warning (contract), because homelab reality
  includes `latest`-style tags, but drift on tag move requires `--force`
  pulls which we do on poll-detected change.

## Consequences

- Reconcile is **serialized** (one at a time — Dokku is a singleton
  resource; a lease row in SQLite + in-process mutex, ADR-0007).
- Every run records: repo sha, per-service outcome, plan summary — feeding
  `topctl history`, metrics, and the diff command.
- TOP manages one shared internal Dokku network (`network:create`, e.g.
  `top-internal`) for `routing.mode: internal` services; attach via
  `network:set ... attach-post-deploy` so scrapes start after checks pass.
- `topctl diff` is exactly the plan computation with zero side effects —
  guaranteeing parity between preview and apply.

## Open questions

- Mono-revision vs per-directory revisions (reconcile only changed
  namespaces on webhook path filter): v0 reconciles the whole repo every
  run (small scale); path-filtered fast paths are an optimization if needed.
