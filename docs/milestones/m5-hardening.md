# Milestone 5 — Operational hardening

> ## M5 record (T-506, 2026-11-03)
>
> T-504 failure-injection pack (`8484589`..`939e588` — kill-mid-deploy,
> reboot; tier-4 scripts). T-505 (fresh-eyes run) and T-507 (release
> engineering) follow T-506. **Status: live-run evidence pending — nothing
> here is a claim that AC rows are green against a real VM.**

## Objective & scope

Make the platform survivable and the operations boring: failure-injection
coverage of the failure matrix, complete public documentation, and release
engineering. Backups are the operator's job (ADR-0013): the runbook carries
the operator recipe, and revival is Dokku-side (restart policy, `dokku
ps:restart`, re-running the installer).

## Non-goals

- Application-data backup automation (ADR-0013 boundary; cookbook only).
- TOP-side backup or restore machinery of any kind.

## Prerequisites

- M4 done.

## Deliverables

- Failure-injection scripts for the failure matrix (and re-runs thereof).
- Docs completion: installation, revival, backups (operator recipe),
  teardown, observability, troubleshooting; README quick-start re-verified;
  ADR statuses → accepted.
- Release engineering: versioned binary releases, checksums, install script
  pinning; contributor docs (how to run tiers 1–4).

## Acceptance criteria (maps to global tests 7, 8, 10)

- [ ] Mid-deploy controller kill → resume/re-diff with consistent state.
- [ ] The failure matrix runs against a real VM: every injection exits
      loud, states stay consistent, and revival needs no TOP-side verbs
      (restart policy / `dokku ps:restart` / installer re-run).
- [ ] A contributor following only public docs installs, reconciles,
      breaks, revives, and uninstalls on a clean VM (fresh-eyes run;
      findings fixed).

## Failure scenarios

- Corrupt SQLite on upgrades → typed error rows, controller exits loud,
  rebuild path documented (`install` + reconcile, idempotent).

## Documentation

- This milestone *is* largely documentation; DoD includes a doc-completeness
  checklist against every contract and runbook.

## Risks & rollback

- Fresh-eyes run finds doc gaps → expected; fix-forward within the milestone.

## Definition of done

All 10 acceptance tests scripted and green; docs complete; v0 tagged.
