# Milestone 5 — Backups, recovery, and operational hardening

> ## M5 record (T-506, 2026-11-03, **pre-owner sign-off**)
>
> Code landed (backlog §Milestone-5, all tiers-1..4 as marked):
> T-501 backup engine (`98ac1f7`, `d1af6f0` — create/verify/prune +
> registry + metrics), T-502 offsite (`58405d3`, `2905fcd` — rsync/SSH
> copy, alarm-not-fail), T-503 recover suite (`7e7dead`,
> `7d45808` — `Offsite.Pull` + restore with the verbatim ADR-0007
> safeguard order + CLI wiring), T-504 failure-injection pack
> (`8484589`..`939e588` — kill-mid-deploy, wedged DB, disk pressure,
> reboot; tier-4 scripts + hermetic shards). T-505 (fresh-eyes run) and
> T-507 (release engineering) follow T-506. **Status: docs-complete
> sweep executed; owner verdicts R1–R8 (consolidated in the backlog)
> and live-run evidence pending — nothing here is a claim that AC rows
> are green against a real VM.** Known doc-accurate gaps at this
> snapshot: `topctl backup` verbs are notImpl stubs (R8 wiring);
> controller has no scheduled-backup jobs seam (R5); offsite per-row
> registry marker is a proposal (R6); restore mechanics proposals R7.

## Objective & scope

Make the platform survivable and the operations boring: automated verified
metadata backups, the full `topctl recover` suite, failure-injection coverage of
the 12 acceptance tests, and complete public documentation.

## Non-goals

- Application-data backup automation (ADR-0013 boundary; cookbook only).
- Off-host backup destination *implementation* beyond one rsync/SSH target.

## Prerequisites

- M4 done.

## Deliverables

- `internal/backup`: scheduled create/verify/prune, offsite copy, registry,
  metrics (ADR-0007, backup-recovery contract).
- `internal/recover` complete: diagnose / restart / redeploy / restore with
  all safeguards (watermark compare, safety snapshot, migration-forward).
- Failure-injection scripts for acceptance tests 7, 9, 10, 12 (and re-run of
  the whole matrix).
- Docs completion: installation, recovery, backups, teardown, observability,
  troubleshooting; README quick-start re-verified; ADR statuses → accepted.
- Release engineering: versioned binary releases, checksums, install script
  pinning; contributor docs (how to run tiers 1–4).

## Acceptance criteria (maps to global tests 7, 9, 10, 12)

- [ ] Mid-deploy controller kill → resume/re-diff with consistent state.
- [ ] `recover diagnose` identifies each injected fault from the matrix
      (wedged DB, dead controller, full disk, bad config) with correct hints.
- [ ] Backup → damage DB → `recover restore` → platform back, watermark
      safeguards proven (restore over newer DB refused without `--force`).
- [ ] A contributor following only public docs installs, reconciles, breaks,
      recovers, backs up, restores, and uninstalls on a clean VM (fresh-eyes
      run; findings fixed).

## Failure scenarios

- Offsite copy failure → alarmed but local backup intact.
- Restore onto schema-newer DB → refused with clear message.
- Backup while reconciling → consistent snapshot (lease-aware timing).

## Documentation

- This milestone *is* largely documentation; DoD includes a docs-completeness
  checklist against every contract and runbook.

## Risks & rollback

- Fresh-eyes run finds doc gaps → expected; fix-forward within the milestone.

## Definition of done

All 12 acceptance tests scripted and green; docs complete; v0 tagged.
