# Milestone 2 — Bootstrap and host lifecycle

## Objective & scope

Safe host entry and exit: `topctl doctor`, `topctl install` (phases 0–4 of the
installer contract: preflight, inventory, gated Dokku install, wiring,
bootstrap deploy), `topctl uninstall` with ownership guarantees, and the Dokku
adapter's app-CRUD slice. First reconciliation hand-off is stubbed (M3 owns
the real loop).

## Non-goals

- GitOps loop, webhooks, diff engine (M3).
- Observability stack (M4). Backups (M5).

## Prerequisites

- M1 done. Tier-3 harness (real-Dokku container) available.

## Deliverables

- `internal/backend/dokku`: `EnsureApp`, `Inspect` (report composition),
  `Stop`, `Destroy`, `HealthyBackend`, `EnsureNetwork` — against real Dokku
  in tier 3.
- `internal/install`: preflight checks, ownership inventory snapshot,
  ledger writes, phased runner with resume, `--dry-run` everywhere.
- `install-top.sh`: thin fetch+exec wrapper (checksum-verified download,
  or build-from-source flag).
- Gated Dokku install path (`--install-dokku` + interactive confirmation +
  printed change list).
- `topctl uninstall` with tiered `--purge-*` flags and the two-key
  (ledger + label) deletion rule.
- `internal/recover` skeleton: `diagnose` works against a bare host.

## Acceptance criteria (maps to global tests 1, 11)

- [ ] On a clean VM (tier 4-lite or tier 3 + scripted VM): `install-top.sh
      <repo>` reaches "bootstrap controller deployed" with Dokku installed by
      the gated path.
- [ ] Re-running `topctl install` is a no-op resume (idempotency proof).
- [ ] Host with a pre-seeded foreign container: full install → full uninstall
      leaves the foreign container running and unchanged.
- [ ] `--dry-run` install and uninstall print complete plans, mutate nothing.
- [ ] `topctl doctor` on a Dokku-less host exits non-zero with actionable fixes.

## Failure scenarios

- Dokku install declined → clean stop with instructions.
- Port 80 occupied pre-install → preflight failure naming the conflict.
- Bootstrap deploy failure → ledger marks `bootstrap-failed`, diagnose
  explains recovery.
- Uninstall attempted while ledger/labels disagree → skip + warn, no deletion.

## Documentation

- `docs/operations/installation.md`, `docs/operations/teardown.md` finalized
  from the contract; minimum host requirements published.

## Risks & rollback

- Dokku bootstrap script changes upstream → pinned version + tested matrix;
  rollback = uninstall path (itself tested).

## Definition of done

Acceptance green in tier 3/4; runbooks reviewed; backlog M2-* closed.
