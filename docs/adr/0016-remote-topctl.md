# 0016 — Remote `topctl`: driving the host from the operator's machine

- Status: **accepted** (2026-11-07, owner approval)
- Date: 2026-11-07
- Needs explicit approval: **yes**
- Grounded in: the shipped executor seam (`internal/exec`), the Dokku adapter's
  shelling discipline (every host action is already a `dokku <verb>` process
  invocation), ADR-0001 (TOP owns orchestration policy; Dokku owns execution),
  ADR-0008/0014 (secret/public boundaries).

## Context

TOP's v0 binaries assume `topctl` runs **on** the managed host: `install`
mutates local packages/state, the Dokku adapter invokes a local `dokku` CLI,
and the state store opens local files (`/var/lib/top/top.db`). The
steady-state driver — the controller — differs: it runs on the host as a
Dokku app by design (ADR-0005), so GitOps reconciliation is already
endogenous. What is missing is the *interactive* path: the operator typing
`topctl diff` / `topctl deploy` / `topctl history` / `topctl doctor` /
`topctl reconcile --now` **from their own machine** against a homelab host —
without installing Go toolchains, cloning the repo, or SSH-ing in manually
and re-typing every command.

The only existing remote mechanism is the Git transport (read-only deploy
keys + the `--init`/`TOP_INIT_TOKEN`一次性 push path). Interactive verbs have
no client/server protocol, and v0 deliberately added none.

## Decision (proposed)

`topctl` gains a **remote operation mode with no new agent**: the operator
target a host with `--host USER@HOST` (or `TOPCTL_HOST`), and every
host-touching action of the invoked verb routes through a single `ssh`
invocation shape per command — the same discipline today's adapter uses for
`dokku`, i.e. argv-transparent, no shell interpolation, bounded timeouts.

1. **Install stays host-local.** `topctl install` touches apt/nginx/systemd
   equivalence zones and requires stdin-context provisioning; it remains a
   run-on-the-host step (documented). The ADR's scope is steady-state
   operational verbs.(`dokku ps:restore`-adjacent bootstraps happen on the
   host per ADR-0001; re-running install is acceptable drudgery at v0 scale.)
2. **Steady-state verbs become host-transparent.** `diff`, `doctor`,
   `deploy`, `history`, `logs`, `reconcile --now`, `uninstall` (confirm-gated
   as today) each resolve their host target FIRST and compose
   `ssh [-o BatchMode=yes] USER@HOST -- <command…>`. State reads happen the
   same way (`ssh cat <dataDir>/top.db` piped through the same store reader
   or, simpler for v0: `ssh <host> topctl history …` — see Open questions,
   OQ-2).
3. **Trust model unchanged from Dokku's own.** The topctl→host channel uses
   exactly the credentials Dokku already trusts: the operator's ssh key or
   the managed/hosted equivalent (`sshcommand` hook user). No TOP daemon, no
   extra port, no TLS cert chore, no secret duplication (ADR-0008's
   secrets-never-leave-the-host carries over: secret *values* continue to be
   read on the host only).
4. **Strict transport discipline** (failure modes chosen over conveniences):
   `-o BatchMode=yes` (never prompt/hang), explicit `ConnectTimeout`,
   host-key verification refuses unknown hosts with a typed error (the error
   suggests `ssh-keyscan`'s manual step; a Silent `StrictHostKeyChecking=no`
   is a `--dangerous-accept-new-host` opt-in, mirroring ADR-0009's consent
   pattern).
5. **One host per invocation.** v0 composites multi-host operations with the
   shell (`for h in ...; do topctl --host $h ...; done`); a built-in fan-out
   is a post-v0 question (it implies parallelism semantics, per-host exit
   aggregation, and a notification story).

## Alternatives considered

- **tiny TOP agent / gRPC control plane on the host** — a second long-lived
  daemon to babysit per node, another auth story, another upgrade
  chore; rejected for v0: Dokku's own trust model is ssh and TOP wants to
  be appear-on-demand, not always-on.
- **topctl as a hosted server (topctl serve) on the host, CLI talks HTTPS from
  the laptop** — heavier (certificates, listening port exercise, (ѿ attack
  surface on the homelab), secrets-in-config pressure; rejected for v0) for
  the identical reasons ADR-0011 rejected `topctl serve`'s session ergonomics
  takeover.
- **HTTP-Land copy of the CLI into the reconciler (`topctl API surface on the
  controller's HTTP server`)** — attractive, but couples CLI lifecycle to the
  controller app AND means a *dead controller cannot accept operator
  commands* — the exact failure mode ADR-0005 impaired ("controllerless
  recovery vocabulary"). ssh-to-host keeps the operator channel
  independent of TOP's own health.

## Consequences

- `internal/exec` gains an ssh-transport config (a `Runner` variant); the
  Dokku adapter, CLI verbs, and the doctor phases thread a `Target` through
  the existing seams. No behavior change when no `--host`/`TOPCTL_HOST` is
  set (the tested local mode stays the default and CI's mode).
- `install`'s docs gain a "run this ON the host" banner; the `--init` /
  deploy-key story is unchanged.
- The testing strategy gains a tier-3 twin: an sshd testcontainer driving
  real verbs remotely (the same shape T-504-era scripts used for offsite
  ssh).
- ADR-0009's "no host mutations without consent" now applies to the ssh
  channel too: remote destructive verbs inherit the same confirm gates —
  nothing extra is loosened or tightened.

## Open questions (owner)

- **OQ-1**: Default `--host` from a saved config (`config.host`) or
  per-invocation-only? Per-invocation-only is the safer v0 default; a saved
  value is convenient but silently couples the CLI to one host in a
  multi-host future. Proposal: per-invocation + a `--host` in config is
  legal but warns when set.
- **OQ-2**: For state reads, prefer (a) pure ssh plumbing (`ssh cat`) with
  the local binary owning logic — no TOP binary needed on the host beyond
  Dokku — or (b) require `topctl` installed on the host and just
  ssh-wrap it (simpler code, but drag a version-skew problem: which topctl
  where?). Proposal: (b) for v0 simplicity, with a typed version-skew error
  claiming `topctl version` mismatch if it differs.
- **OQ-3**: Does `deploy`(self) of the controller stay human-gated-only over
  this channel identically (yes per ADR-0005; noted here so remote mode
  doesn't look like a plan to soften the self-gate).
- **OQ-4**: `logs` streaming semantics for ctrl-C over ssh (v0: plain ssh
  invocation inherits the local terminal's SIGINT naturally; document it
  rather than engineering).

## Pre-approval notes

Owner approved 2026-11-07 ("looks good for now"): ADR accepted as drafted;
Open questions OQ-1 (per-invocation-only `--host` for v0), OQ-2 (option
(b): ssh-wrapped host-side topctl with a typed version-skew error),
OQ-3 (self-deploy gate unchanged) and OQ-4 (plain ssh log semantics)
resolve as the proposals stated. Implementation milestone M6 + backlog
T-601..T-604 are the delivery plan; CI publication of the release
artifacts landed in the same wave (`.github/workflows/release.yml`).
