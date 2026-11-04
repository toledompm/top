# TOP — Toledo's Opinionated Platform

**GitOps-style continuous reconciliation for [Dokku](https://dokku.com).**
Declare your workloads in Git; TOP keeps a single-node Dokku host converged to
that desired state — like Flux, but for a Dokku box instead of Kubernetes.

> **Status: v0-feature complete (M5); pre-tag.** Implemented across
> M1–M5: the `topctl` CLI, the spec/plan/store/exec/secrets/obs
> foundation, install/uninstall, the GitOps controller (webhook + poll
> convergence, drift healing, adoption handoff), TOP's own core +
> Prometheus/Grafana observability as reconciled specs, the independent
> recovery path (including real `recover restore` with the ADR-0007
> safeguard order), and the backup engine with rsync/SSH offsite copy
> and failure-injection coverage (`test/e2e/t504-*.sh`). Before the v0
> tag: owner verdicts R1–R8 + live-run sign-offs
> ([backlog](docs/tasks/backlog.md)), release engineering T-507
> (versioned binaries, checksums, `install-top.sh` pinning). One honest
> gap: `topctl backup create/list/verify` are still notImpl stubs (R8);
> the engine is real and `recover restore` is real. License: **MIT**
> (`LICENSE`).

## What it does

- You describe workloads in a Git repository as declarative **ServiceSpecs**.
- TOP's controller (itself a TOP-managed Dokku app) fetches that repo on a
  schedule and on authenticated webhooks, computes a diff against the host,
  and applies it through Dokku.
- Drift is detected and corrected; every operation is recorded with a stable
  ID, state, and explainable outcome.
- `topctl` CLI gives you `status`, `diff`, `reconcile`, `deploy`, `recover`,
  `backup`, and `uninstall` — including an **independent recovery path** that
  works even when TOP itself is down.
- TOP's own core services and the Prometheus/Grafana observability stack are
  ordinary TOP-managed workloads, bootstrapped by a small, explicit installer.

## What it is not

- Not multi-node, not HA, not Kubernetes-compatible (see `docs/adr/0001`).
- Not a replacement for Dokku's deployment engine — Dokku stays the runtime.
- Not a secrets platform, workflow engine, or general-purpose orchestrator.

## Documentation map

| Area | Where |
| --- | --- |
| Architecture overview & diagrams | [`docs/architecture/overview.md`](docs/architecture/overview.md) |
| Decisions (ADRs) | [`docs/adr/`](docs/adr/README.md) |
| Contracts (ServiceSpec, backend, store…) | [`docs/contracts/`](docs/contracts/README.md) |
| Install / operate / recover | [`docs/operations/`](docs/operations/) |
| Milestones & acceptance criteria | [`docs/milestones/`](docs/milestones/README.md) |
| Implementation backlog | [`docs/tasks/backlog.md`](docs/tasks/backlog.md) |
| Agent/contributor conventions | [`AGENTS.md`](AGENTS.md) |

## Quick start

```bash
./install-top.sh github.com/example/my-platform-repo
```

`install-top.sh` only locates a `topctl` binary (PATH → pinned release →
`go build`) and forwards everything to `topctl install`, which preflights,
plans, and approval-gates each phase; the installer still needs a
`--deploy-key` + `--known-hosts` pair. Prerequisites and the phase
walkthrough: [`docs/operations/installation.md`](docs/operations/installation.md).
`topctl dev` variant: on macOS, only the development tier runs (doctor
warns, never installs).

## Contributing

Read [`AGENTS.md`](AGENTS.md) first — it carries the repo's hard rules
(public/private boundary, the one authoritative backlog in
`docs/tasks/backlog.md`, conventions, and the bend laws gate before any
change to `laws/`, `internal/domain`, or `internal/plan`). New work is added
as tasks there, not in side files; durable decisions go in
[`docs/adr/`](docs/adr/README.md). Run `make help` to see the local gates
(`make test`, `make vet`, `make laws`, `make hygiene`).
