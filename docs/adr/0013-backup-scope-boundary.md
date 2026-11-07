# 0013 — Backup scope: metadata vs application data

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

"Backups" can mean two very different things: TOP's own metadata (SQLite —
platform state, deployment records) and the data of the workloads TOP manages
(volumes, databases). Blurring them produces dangerous false confidence.

## Decision

**In v0 backups are the operator's job, for both scopes.** TOP ships no
backup mechanism of its own: no scheduler, no CLI verbs, no off-host copy
wiring. The product says so plainly, and the canonical recipes live in
`docs/operations/backups.md`.

1. **TOP's metadata** (the SQLite DB with desired/observed state, the
   operation ledger, checkpoints; plus the redacted config) is small and —
   more importantly — *reconstructible*: re-running `topctl install` and
   letting reconcile converge (which is idempotent) rebuilds the
   recorded state. The operator's routine copy of the SQLite file under
   the `dokku storage` mount plus the recipes in the runbook are
   sufficient; SECRET values are never in that DB (ADR-0008) and get
   re-provisioned out-of-band.
2. **Application data is the operator's, unambiguously.** Dokku storage
   mounts, app databases, and volume contents are never TOP's to back up.
   `docs/operations/backups.md` documents the why and provides a cookbook
   (filesystem snapshots, `pg_dump`-style per-app jobs as ordinary
   TOP-managed cron workloads, etc.). The extension point is noted only:
   a future service-data backup feature could orchestrate per-service
   dumps via declared hooks in ServiceSpec (e.g.
   `spec.backups.preDumpCommand`); nothing is reserved for it today.
3. **What is and isn't rebuildable is documented per scenario**: TOP DB
   lost → wipe the state file, re-install, reconcile re-converges the
   apps; app volume lost → TOP does not help; restore from your
   app-level backup, then `topctl reconcile`.

## Alternatives considered

- **Full-scope backups (metadata + app data) in v0** — every app stores data
  differently; a generic mechanism is either useless (raw volume tars) or a
  product of its own. Explicitly rejected by the brief's non-goals.
- **TOP-managed metadata backups (scheduler + CLI + engine) in v0** — the
  metadata is cheap to rebuild, and an operator with one host already has a
  backup habit. Explicitly rejected by owner ruling; revisit only if
  multi-host scale arrives with it.
- **No backups at all, not even operator recipes** — the runbook's cookbook
  (both scopes) costs documentation only and is what a one-operator homelab
  actually needs.

## Consequences

- Release notes and README carry the one-line boundary: "your apps' data is
  yours — TOP ships no backup machinery."
- The runbook's recipes include re-provisioning secrets and app data as
  separate, manual steps.
