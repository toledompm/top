# TOP architecture overview

## Executive summary

TOP is a single-node GitOps platform: **declarative workload specs in Git,
continuously reconciled onto a Dokku host** — Flux's operating model at
homelab scale. One Go binary (`topctl`) serves as CLI, controller, installer,
and installer. The controller runs *on* Dokku as an ordinary TOP-managed
app (`core-top-controller`), keeps its metadata in SQLite, and drives Dokku
exclusively through its CLI. Dokku remains the runtime: builds, containers,
proxy, storage, reboot survival. TOP adds desired state, planning, drift
correction, and history; revival works when TOP itself is down. TOP's own
core services and the Prometheus/Grafana stack are managed
exactly like user workloads; the only special case is that applying a new
*controller* revision is human-gated (ADR-0005).

Everything here is decided in the ADRs ([`docs/adr/`](../adr/README.md));
this document is the map.

## Component diagram

```
                 ┌────────────────────── Git (source of truth) ──────────────────────┐
                 │  platform repo:  core/*.yaml  observability/*.yaml  apps/*.yaml    │
                 └───────▲───────────────────────────────────────────┬───────────────┘
        deploy key (RO), │ pinned known_hosts                        │ webhook (HMAC, 202+enqueue)
        git ls-remote /  │ fetch                                     ▼
        archive          │                              ┌────────────────────────────┐
                 ┌───────┴───────┐                        │  HTTP: /webhooks/github     │
                 │  GitSource    │                        │  /healthz  /metrics         │
                 └───────┬───────┘                        └────────────┬───────────────┘
                         │ sha + spec set                               │ enqueue (dedup)
                         ▼                                               ▼
┌───────────────────────────────── TOP controller (Dokku app: core-top-controller) ──┐
│                         Reconciler: validate → order → diff → apply → verify        │
│                               │                ▲                                    │
│                               ▼                │ inspect (JSON reports)             │
│                    PlatformBackend (dokku adapter, dokku CLI only)                  │
│                               │                                                     │
│  StateStore (SQLite, WAL)                                                           │
│  operations/ledger/checkpoints/deliveries                                           │
└───────────────────────────────┬─────────────────────────────────────────────────────┘
                                 │ dokku CLI (exec layer: argv, timeouts, redaction)
                                 ▼
┌──────────────────────────────────── Dokku host ─────────────────────────────────────┐
│  core:    core-top-controller (+ sqlite volume on dokku storage)                    │
│  observability: prometheus · grafana · node-exporter · cadvisor                     │
│  <user namespaces>: user workloads                                                  │
│  nginx proxy (80/443) · docker networks (incl. top-internal) · ps:restore on boot   │
└─────────────────────────────────────────────────────────────────────────────────────┘

   topctl CLI on the host:  status · diff · reconcile · deploy · history · logs
                         install · doctor · uninstall
```

## Key flows

**Steady-state reconcile (webhook)**: GitHub push → HMAC-verified POST →
delivery dedup → enqueue → controller `ls-remote` resolves sha → fetch →
validate specs → per service: inspect via Dokku JSON reports → diff → apply
(`git:from-image`/`git:sync --build-if-changes`, batched `--no-restart`
config) → readiness gate (`builds:report` + `checks:run`) → record operation,
checkpoint, metrics. Poll (60 s) repairs missed webhooks; hourly full
reconcile sweeps drift.

**Manual preview**: `topctl diff` runs the same plan computation with zero side
effects; exit code 1 = drift. `topctl reconcile --now` applies.

**Rollback**: operator pins the service to its last-known-good revision (or
`topctl deploy ns/name --revision <old>`); Dokku's zero-downtime checks protect
the switch exactly as for a forward deploy; previous images retained via
`builder:set skip-cleanup true`.

**TOP is down**: workloads keep running (plain Docker containers; Dokku has
no daemon; `ps:restore` handles reboots). Revival is Dokku-side: the
controller's restart policy, `dokku ps:restart`, `dokku ps:rebuild`, or a
re-run of `topctl install`; `topctl doctor` inspects Dokku + SQLite + config
directly. TOP's metadata DB is reconstructible by re-running operations
(install + reconcile, idempotent).

**Install**: `install-top.sh <repo>` → preflight → ownership inventory →
(gated) Dokku install → secrets/git wiring → one-time bootstrap deploy of
the controller → first reconciliation adopts everything, including the
controller itself. From then on, Git governs.

## Assumptions register

**Verified (official Dokku 0.38.x docs, fetched 2026-10-27)** — no daemon;
app lifecycle/deploy/config/checks/storage/network/ports/logs/builds commands
and their JSON flags; zero-downtime ordering and old-release preservation;
`config:set` redeploy behavior; `git:from-image` idempotency + local-tag skip;
`git:sync` build flags; `ps:restore`/event-listener reboot model; documented
state dirs; supported OS (Ubuntu 22.04/24.04/26.04, Debian
11+, x86_64/arm64, ≥1 GB RAM); current stable v0.38.31. Detailed notes with
URLs are kept in session research; the ADRs encode the conclusions.

**Verified (library/tooling research, 2026-10-27)** — modernc.org/sqlite
(WAL pragmas); goose v3 maintenance posture; go-git v5 frozen /
v6 beta → git CLI chosen; `gopkg.in/yaml.v3` archived → `go.yaml.in/yaml/v3`;
robfig/cron unmaintained; official `dokku/dokku` Docker image supports
sock-mounted sibling-container operation; testcontainers-go active.

**Inferred (documented parts combined, flagged)** — failed checks keep the
old release serving (direct consequence of documented deploy ordering, not a
single sentence); `apps:destroy` preserving storage host dirs is
source-confirmed, not documented; the full Dokku label set is
stable-but-undocumented API (we rely only on documented
`com.dokku.app-name`).

**Unknown / to validate in milestones** — synthesized-archive healthcheck
injection end-to-end (M3); Prometheus scrape auth beyond network isolation
(deferred, ADR-0010); Dokku apt-purge leftover enumeration (M2 uninstall
testing); exact small-host Prometheus sizing (M4).

## Research appendix (one paragraph)

Two structured research efforts on 2026-10-27 ground this plan: (1) a
Dokku-docs pass over 18 capability questions against dokku.com (every
mapping table in `docs/contracts/platform-backend.md` cites verified
commands; surprises encoded in ADR-0001/0002 include `config:set` redeploy
semantics, `git:from-image` local-tag caching, DIY rollback, event-listener
interaction, and docs-drift risk → minimum-version pinning); (2) a Go-stack
pass (SQLite driver + migrations, git access, webhooks, CLI, logging,
metrics, reconcile loop, exec safety, YAML library, testing
tiers) — accepted choices are in the ADRs; the dependency list lands in
Milestone 1's `go.mod`.
