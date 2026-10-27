# 0013 — Backup scope: metadata vs application data

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

"Backups" can mean two very different things: TOP's own metadata (SQLite —
platform state, deployment records) and the data of the workloads TOP manages
(volumes, databases). Blurring them produces dangerous false confidence.

## Decision

**v0 scope: TOP backs up its own metadata only** (ADR-0007), and the product
says so loudly.

1. `topctl backup`/`topctl recover restore` concern exclusively the TOP SQLite DB
   (plus the redacted config and the resource ledger, which live in/alongside
   it). Secret *values* are excluded (ADR-0008).
2. **Application data is out of scope for v0 automation.** Dokku storage
   mounts, app databases, and volume contents are the operator's
   responsibility; `docs/operations/backups.md` documents *why* and provides
   a cookbook (filesystem snapshots, `pg_dump`-style per-app jobs as ordinary
   TOP-managed cron workloads, etc.) without shipping a mechanism.
3. **What is and isn't recoverable is documented per scenario** in
   `docs/operations/recovery.md`: e.g. "TOP DB lost → restore from backup,
   reconcile re-converges apps" vs "app volume lost → TOP does not help;
   restore from your app-level backup, then `topctl reconcile`".
4. The extension point is named: a future `BackupProvider` implementation
   could orchestrate per-service data backups via declared hooks in
   ServiceSpec (e.g. `spec.backups.preDumpCommand`). Not in v0; the contract
   reserves the field namespace so adding it is non-breaking.

## Alternatives considered

- **Full-scope backups (metadata + app data) in v0** — every app stores data
  differently; a generic mechanism is either useless (raw volume tars) or a
  product of its own. Explicitly rejected by the brief's non-goals.
- **No backups at all in v0** — TOP's DB is small and its loss is recoverable
  by reconstruction, but deployment history + ledger + checkpoints are
  operationally valuable and cheap to protect. Metadata-only is the right
  scope.

## Consequences

- Release notes, README, and the CLI help text all carry the one-line
  boundary: "TOP backs up TOP. Your apps' data is yours."
- The recovery runbook's disaster scenario includes re-provisioning secrets
  and app data as separate, manual steps.

## Open questions

- Should the backup include a *names-and-checksums* manifest of secret store
  contents to simplify re-provisioning? Proposal: yes (names + SHA-256, no
  values); confirm in Milestone 5.
