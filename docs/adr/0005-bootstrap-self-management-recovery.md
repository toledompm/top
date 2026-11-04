# 0005 — Bootstrap, self-management, and the independent recovery path

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes**

## Context

TOP must manage its own core services and observability stack as ordinary
TOP-managed workloads (self-management), yet there is a bootstrap boundary:
nothing can reconcile itself before a reconciler exists. Recovery must also
work when TOP is broken. Both need to be designed explicitly, not emerge
by accident.

## Decision

**Topology.** The TOP controller runs *on Dokku* as the app
`core-top-controller`, with its SQLite state on a Dokku-managed persistent
storage mount. Same delivery mechanism as every other workload.

**Bootstrap.** `./install-top.sh <git-repo-url>` is a thin, inspectable shell
script that installs the `topctl` binary and runs `topctl install <git-repo-url>`.
`topctl install`:

1. preflights the host (OS, Docker, Dokku presence/health, ports, resources);
2. records an **ownership inventory** of pre-existing host state (ADR-0009);
3. performs a one-time **bootstrap deploy** of the controller using direct
   Dokku commands and a pinned controller image;
4. registers the Git repo, triggers the **first reconciliation**, which
   adopts the bootstrap deployment into the desired-state model and manages
   everything (including `core-top-controller`) from Git from then on.

**Repo initialization at install** (owner-directed, M0 review): if the given
repository **does not exist or is empty**, `topctl install --init` creates it
from TOP's embedded platform-repo template (the same content published in
`examples/platform-repo/`: `core/top-controller.yaml`, `observability/*`,
README) — TOP renders the template locally, creates the remote repo where
the provider supports it, and pushes the initial commit with a one-time
write-capable credential supplied for the occasion (never stored; steady-state
sync uses the read-only deploy key, ADR-0008). If the repo **already exists
with content**, TOP changes nothing in it — it just registers and starts
syncing. Without `--init`, a missing/empty repo is a clean preflight error
with instructions. This keeps the brief's one-line entry point working for
both the greenfield and the bring-your-own-repo cases.

The bootstrap path is small, deterministic, documented, and separate code from
the reconciliation loop. It is exercised in e2e tests (Milestone 4).

**Self-update is human-gated in v0.** The controller reports drift on its own
ServiceSpec but does *not* apply new revisions of itself automatically.
Applying a controller revision requires `topctl deploy core/top-controller`
(or `--include-controller` on reconcile) — an explicit act. Rationale below.

**Independent recovery.** `topctl recover` is a set of CLI subcommands that run
on the host and depend only on: the Dokku CLI, the filesystem, and the SQLite
file — never on the controller, Prometheus, or Grafana being healthy:

- `topctl recover diagnose` — checks controller app state, recent deploy logs,
  SQLite integrity/openability, config validity, Dokku health; prints
  actionable findings.
- `topctl recover restart` — restarts the controller via Dokku.
- `topctl recover redeploy [--revision <sha>]` — redeploys the controller
  directly via Dokku (default: last known-good revision from the state DB,
  falling back to the bootstrap-pinned image).
- `topctl recover restore --backup <file>` — restores metadata with safeguards
  (ADR-0007).

## Alternatives considered

- **Controller as a systemd unit, not a Dokku app** — simpler bootstrap, but
  then self-management is a lie: the most important workload would be managed
  by a different mechanism than everything else. Rejected.
- **Fully automatic self-rolling updates** (Flux-style) — a bad controller
  deploy that wedges the reconciler kills all future reconciliation,
  including the fix. With one operator and no HA, the risk/benefit is wrong
  for v0. The health-gated manual path keeps self-management honest (spec,
  diff, drift, deploy mechanics all identical) while making the blast radius
  a conscious choice. Revisit after Milestone 5 hardening.
- **Installer keeps managing core forever** — explicitly forbidden by the
  product vision; the installer must hand off and shrink to bootstrap+recovery.
- **Recovery via a second tiny orchestrator** — rejected; recovery is read
  inspection + a handful of direct Dokku operations, nothing more.

## Rationale and trade-offs

Dokku apps are plain Docker containers with restart policies; Dokku itself is
stateless CLI tooling. So `core-top-controller` being managed by TOP does not
create a circular dependency at runtime — when the controller is down,
existing workloads keep running, and Dokku remains fully operable by hand.
The circularity exists only at *deploy time*, which the bootstrap path and
human-gated self-update cover.

Trade-off: someone must run `topctl deploy core/top-controller` to upgrade TOP.
That is one documented command; release notes will say so.

## Consequences

- The controller image must be published (registry) or buildable on-host from
  the repo; bootstrap pins a specific version.
- State DB must live on mounted storage so `apps:destroy`-free redeploys and
  `ps:rebuild` retain it; teardown policy for that volume is explicit
  (ADR-0009).
- First reconciliation must *adopt* the bootstrap controller deployment
  (same app name) rather than error on drift.
- Acceptance test (Milestone 4): kill the controller, prove workloads +
  monitoring keep running, prove `topctl recover` brings the controller back.

## Open questions

- Should `--include-controller` still require a typed confirmation on a
  production-flagged install? Decide in Milestone 3. *(Resolved during M4:
  yes — the confirm gate applies; `internal/cli` `SelfDeployApproved` +
  `docs/contracts/cli.md` row, ADR-0009.)*

## Amendment 2026-11-04 (owner verdict R3 — adopted-by-op annotation)

The phase-5 first-reconciliation provenance lives on the **operation
record**, not the ledger row. The adopting install op record carries the
details keys `adoptedByOp`, `firstRunOp`, `firstRunRevision`,
`firstRunStatus` (`internal/install/adopt.go`); the ledger handoff itself
is `store.MarkManaged(kind, identifier)` — a pure state conversion whose
`(kind, identifier)` surface leaves no room for annotation columns, which
is the constraint this settles. `MarkManaged` stays idempotent and
refuses pre-existing rows (ADR-0009 spirit); re-adoption guards on the
op-record keys instead. The installer contract's phase-5 wording takes the
matching composition-root clause (the install CLI runs the first pass via
`Options.FirstRun`; the adopted controller takes over afterwards).
