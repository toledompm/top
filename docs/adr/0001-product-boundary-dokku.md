# 0001 — Product boundary: what TOP owns vs what Dokku owns

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes**
- Grounded in: Dokku 0.38.x official docs (see `docs/architecture/overview.md` research notes)

## Context

"Flux for Dokku" only works if the division of labor is crisp. Verified facts
that shape it:

- **Dokku runs no daemons.** It is CLI tooling over Docker + nginx + file
  state (`/home/dokku`, `/var/lib/dokku`). App containers are plain Docker
  containers; a broken/absent Dokku CLI does not stop them.
- Dokku **explicitly does not tolerate out-of-band Docker mutation** of its
  apps, and its pre-deploy cleanup deletes dead/exited containers carrying
  the `dokku` label.
- Dokku has **no deployment API, no rollback command, no desired-state
  concept, and no drift detection**. It has excellent primitives: idempotent
  `git:from-image` deploys, `git:sync --build[-if-changes]`, zero-downtime
  checks with old-release preservation on failure, JSON reports
  (`ps:report`, `builds:report`, `apps:report`, `git:report`), and boot
  recovery via an init hook running `ps:restore` plus the
  `dokku-event-listener` service.
- Docs drift between releases is real (0.38.0 alone moved env storage,
  reworked storage, added build records).

## Decision

**TOP owns**: desired state (Git), reconciliation, planning/diff, deployment
history and operation tracking, readiness *interpretation*, rollback *policy*
(re-issue previous desired ref), and platform policy (namespaces, ordering,
protected resources).

**Dokku owns**: app execution, builds, image lifecycle, container lifecycle,
proxy/routing, TLS termination, storage mounting, restart/reboot survival of
workloads, and deploy-time health checking.

**Interaction rules (hard constraints on all TOP code):**

1. TOP manipulates Dokku apps **only through the `dokku` CLI** (via the exec
   layer, ADR-0006). It never starts/stops/labels Dokku app containers
   through the Docker API directly.
2. TOP never applies the `dokku` label or `com.dokku.*` labels to anything
   (Dokku's GC would eat our resources). TOP's own labels use `top.dev/*`
   (ADR-0009).
3. TOP treats Dokku's host-reboot behavior (`ps:restore` init hook +
   `dokku-event-listener`) as the workload-availability mechanism and does
   not build a competing one. It tolerates the documented restore window in
   which requests can mis-route (no alerting on it).
4. TOP composes readiness from documented reports
   (`ps:report --format json`, `builds:report --build-status`, `checks:run`,
   `network:report`) instead of parsing human deploy output.
5. **Minimum supported Dokku: 0.38.x**, pinned in preflight; upgrade guides
   are re-read on each supported-version bump (docs drift is a known hazard).

## Alternatives considered

- **Drive Docker directly, keep Dokku only for builds** — fights Dokku's
  documented constraints (out-of-band mutation, label GC) and doubles the
  state model. Rejected.
- **Wrap every Dokku command as a TOP command** — explicitly out of scope in
  the brief; TOP exposes platform-level operations, not a dokku CLI mirror.
- **Kubernetes instead** — violates the product vision and non-goals.

## Consequences

- "What happens to workloads when TOP is down" = "nothing" (Dokku apps are
  plain containers), which keeps the revival design (ADR-0005) small.
- Rollback is a TOP-level concept implemented as *re-deploy previous desired
  revision* — natural in GitOps, and it leans on `git:from-image`'s
  idempotency and `source-image` tracking.
- The PlatformBackend contract (`docs/contracts/platform-backend.md`) is the
  only place Dokku command knowledge lives.

## Open questions

- Do we support the k3s scheduler later? Not in v0; the backend contract is
  the seam.
