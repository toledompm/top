# Milestone 2 — Bootstrap and host lifecycle

> ## M2 review (T-211, 2026-10-27, closure)
>
> **Fleet result**: T-201..T-210 delivered. The tier-3 real-Dokku harness
> ran a live 4/4 pass on Dokku 0.38.32 in a disposable container
> (HealthyBackend, EnsureNetwork idempotent, EnsureApp + never-deployed
> mirror, full lifecycle with `httpd:2.4-alpine` via `git:from-image`),
> plus golden-argv coverage of `DeployImage`/`EnsureStorageEntry`/
> `AttachStorage` (the installer's bootstrap extension, "extend-not-fork").
>
> **Gate matrix (full tree, final)**: vet OK · 16 packages race-clean ·
> conformance (tag) OK · shim-tier OK · laws gate green (ALL PROOFS CHECK +
> bridge) · gofumpt clean · golangci-lint 0 issues · hygiene pass ·
> `install-top.sh --check` verified against the real CLI on the dev Mac
> (doctor's WARN-profile dev-run semantics — recorded, not hidden).
>
> **Acceptance status (M2-capped)**: #1 install-from-git — implemented
> through phase-4 bootstrap with adoption-wing M3/M4; #11 foreign-resource
> survival — faked-host green (`TestUninstallRemovesOwneKeepsForeign`,
> two-key law + typed refusal reasons); real-host e2e stays gated in the
> tier-3/4 harness (M5 exercises the full matrix per plan). #12's
> contributor path: `install-top.sh --check` runs read-only on a bare dev
> Mac — portable confirmed.
>
> **Amendments applied during M2** (documented in-place):
> - ADR-0006: the recovery/bootstrap direct-dokku carve-out (recover +
>   install phase-2 exec dokku directly; inventory reads for the same
>   reason).
> - ADR-0012's CI: conformance joins after M2; the laws gate + a separate
>   optional CI job for `make laws` shipped in M1.5.
> - `internal/install`'s health-wait pacing has a documented env knob
>   (`TOP_INSTALL_POLL_INTERVAL_MS`, install-local; the hermetic-CLI test
>   and contributors use it; recorded in the package README).
> - The `default-` namespace prefix collision flag (T-204 review) is
>   recorded as the two-key-rule edge in ADR-0009's consequences at the
>   T-204 finding level — naming layer alone NEVER deletes.
>
> **Owner open items**: none blocking M3. The M2 DoD now requires the
> owner's explicit sign-off per the milestone plan.


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
