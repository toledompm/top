# internal/install — phased installer, bootstrap deploy, and teardown

Purpose: `topctl install <repo>` and `topctl uninstall` — the only
entrypoints that put TOP *on* a host or *off* it. Contract:
[`docs/contracts/installer.md`](../../docs/contracts/installer.md);
decisions: [ADR-0005](../../docs/adr/0005-bootstrap-self-management-recovery.md)
(bootstrap toplogy, human-gated self-update),
[ADR-0009](../../docs/adr/0009-host-ownership-safeguards.md) (ownership +
approval gates). TOP's *own* configuration lives in `internal/config`;
reconciliation never runs from this package (that's the M3 controller).

## Phases (implemented)

| Phase | Contents | Gate |
| --- | --- | --- |
| 0-preflight | `internal/doctor.Run` + installer-specific checks (repo URL shape, deploy-key file visibility) — aborts before any mutation | read-only |
| 1-inventory | `internal/inventory.Snapshot` + `CreateLedgerFromSnapshot` — the pre-existing host state is ON RECORD before TOP creates anything | read-only |
| 2-dokku-setup | verify Dokku presence/health/version (≥ 0.38); `--install-dokku` runs the upstream bootstrap — `--confirm` required; sudo is *printed, never escalated* (ADR-0009) | mutating, gated |
| 3-platform-wiring | create `$dataDir/{,secrets,git,backups}` (dirs 0700, files 0600), `EnsureNetwork(top-internal)`, deploy-key + pinned known_hosts into `$dataDir/git/` | mutating, gated |
| 4-bootstrap-deploy | storage entry + mount for SQLite (`top-metadata`) → app `core-top-controller` ← pinned image (`--controller-image/--controller-tag`, defaults `ghcr.io/example/top/topctl:bootstrap-pinned`-format), bounded health wait; ledger marks `bootstrap-owned` | mutating, gated |
| 5-first-reconcile | ONE reconciler pass over the seeded repo specs + the **ADR-0005 ownership handoff**: the controller app's ledger row converts `bootstrap-owned → managed` (`store.MarkManaged`). Adoption itself is a state conversion — no Dokku verb, no redeploy. Through the `Options.FirstRun` seam (`FirstRunner` in `adopt.go`; the composition root composes `gitsource` + `spec.LoadDir` + `reconcile.Run`). Missing controller spec, no handoff wired, or nothing recorded to adopt → **info finding**, the deployment stays `bootstrap-owned` and the next install **redoes phase 5** (completion is keyed on the adoption record, not the op state). Pre-existing row → typed `ErrAdoptionRefused`. Non-green first run → phase fails, the row is NOT flipped. Re-running after a successful adoption re-adopts nothing (op-record guard; `details["adoptedByOp"]` + `details["firstRunOp"]` live on the phase op). | mutating (store only), gated via `--confirm` |
| 6-report | created/adopted/left-alone + secrets-to-provision + next commands; exits 0 | read-only |

Teardown tiers (`topctl uninstall`): everything is **kept by default**;
`--purge-data`, `--purge-secrets`, `--uninstall-dokku` each add their own
tier, each with its own confirmation (no yes-to-everything — ADR-0009).
Deletions follow the **two-key rule** via
`internal/inventory.Deletable` (ledger state ∧ fresh label/prefix re-read)
— foreign resources are reported in the final diff and never touched
(acceptance test 11).

## Guarances tested here

- **dry-run zero-mutation**: `--dry-run` prints the full plan and runs no
  mutating verb (asserted on the call log + fs diff).
- **anti-redeploy adoption**: the phase-5 handoff issues no host verb — an
  install whose first run confirms the spec matches the bootstrap
  deployment shows exactly ONE controller `git:from-image` (phase 4) in
  the full call log and zero rebuild/restart/convergence verbs
  (`TestInstallPhase5AdoptsAndRunsFirstReconcile`,
  `TestInstallAdoptionRealPipeline` with the real gitsource+spec+reconcile
  composition over a local file:// repo).
- **idempotent resume**: an interrupted install (kill mid-phase-4, or a
  failed bootstrap with the `bootstrap-failed` ledger marker) re-runs to
  completion; completed phases are skipped via the operations records.
- **gate matrix**: non-TTY without `--confirm` → exit 2 with the exact
  guidance; `sudo` missing/password-required → the exact `sudo` command is
  printed and nothing more happens.

## Pacing knob

The health-wait poll interval is configurable via install-local env:
`TOP_INSTALL_POLL_INTERVAL_MS` (default 2000 ms). The CLI hermetic tests
and contributors on slow environments lower it to 1; it affects pacing
only, never correctness.

## Known fences for later milestones

## `--init`: seeding an empty platform repo (T-212)

`topctl install <repo> --init` pushes ONE initial commit into a platform
remote the operator created EMPTY, seeding TOP's embedded platform-repo
template (`internal/install/platformrepo`, symlinked at
`examples/platform-repo` — the same content, single source of truth). The
remote itself is never created via a provider API (deliberate v0 deferral of
ADR-0005's "provider permitting" clause); an unreachable remote under
`--init` is a clean typed refusal (`ErrRemoteUnreachable`). A non-empty
remote under `--init` refuses with the remote's evidence and never forces,
merges, or overwrites (`ErrRepoNotEmpty`); a re-run recognizes its own seed
and resumes.

Credentials (ADR-0008): https remotes take a one-time write token from the
`TOP_INIT_TOKEN` environment ONLY, carried in the child env through a 0700
askpass shim deleted after the push — never argv, never git config, never on
disk, never logged. ssh remotes authenticate with the operator's deploy-key
FILE PATH inside `GIT_SSH_COMMAND`; if that key is read-only the push fails
loudly on the provider's stderr — switch the remote to https + `TOP_INIT_TOKEN`
instead. The steady-state transport stays the read-only deploy key.
- **First reconciliation / adoption** (contract phase 5) lands M3/M4: until
  then the bootstrap deployment stays `bootstrap-owned`.
- Deploy-key *generation* (vs operator-supplied) is M3.
- Controller self-deploy gating is T-406/M4 (never auto-applied — ADR-0005).
- The bootstrap deploy's e2e (real Dokku via the tier-3 harness) is wired
  in `internal/backend/dokku/integrator_test.go`; CI runs tiers 1–2 only.

Testing tiers: FakeRunner golden-argv per phase + full-sequence
integration over temp SQLite; **no real Dokku in this package's default
tests** — never point fixtures at a shared host.
