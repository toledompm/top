# 0009 — Host ownership, install/teardown safeguards, and approval gates

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes**

## Context

TOP runs on a real host that may already serve other workloads. Installation
must be idempotent and ownership-aware; teardown must never remove resources
it cannot positively identify as its own. The brief also mandates explicit
human approval for destructive/broad host actions.

## Decision

**Ownership model — three identification layers (belt, suspenders, receipt):**

1. **Naming**: every TOP-created Dokku app is `<namespace>-<name>` with
   namespaces registered in TOP config; the controller's own app is
   `core-top-controller`.
2. **Labels**: every Docker resource TOP creates (via Dokku where possible,
   directly only where Dokku offers no hook) carries
   `top.dev/managed=true`, `top.dev/namespace`, `top.dev/service`.
   Unlabeled-but-named matches are *adopted* only via the bootstrap
   inventory, never silently.
3. **Inventory ledger**: at install, TOP records a host inventory (containers,
   networks, volumes, listeners, dokku state) plus every resource it creates,
   in SQLite (`resources` table). `topctl uninstall` removes exactly what the
   ledger lists *and* that still carries our labels/name-prefix — both must
   agree, else skip with a warning.

**Preflight (`topctl doctor` / `topctl install` first phase)** verifies, and prints
fixes for: supported OS/arch; Docker present, daemon reachable, version floor;
Dokku present and healthy *or* absent (see below); ports 80/443 free if the
proxy will be enabled; sufficient disk/RAM headroom (documented minimums);
no conflicting resources with TOP-reserved names.

**Dokku install is a guided, approval-gated step.** If Dokku is absent,
`topctl install --install-dokku` runs the documented upstream install *with
explicit human confirmation*, after printing exactly what it will change
(apt packages, nginx, a `dokku` user, sshcommand hooks). Default (no flag):
stop with instructions. Rationale: installing Dokku mutates shared host
networking (nginx on 80/443) and adds a git-over-SSH path; that class of
change is on the brief's approval list, so the tool asks rather than assumes. (Owner reviewed: the gate stays, and the `--confirm` flag is the sanctioned way through it — see below.)

**Approval gates are explicit consent markers, not interactive friction**
(amended per owner review). Every gated operation has two equivalent paths:
an interactive prompt (the default on a TTY) and a deterministic
non-interactive one (`--confirm` plus whatever specific destructive flag
applies, e.g. `--purge-data`). Passing the flags *is* the approval —
deliberate, auditable, and logged — so agents and CI can fully script
tear-down-and-reinstall cycles against test environments (ADR-0012 confines
destructive e2e to disposable hosts by default). There is no single
"yes-to-everything" switch: each destructive tier keeps its own flag.
Gated operations:
Dokku install/uninstall; deleting any volume or storage dir; firewall/proxy
changes; touching any resource not in the ledger; overwriting the SQLite
state file; `topctl uninstall`; `--prune` deletions. Where the operator is
not root, TOP prints exact `sudo` commands rather than attempting escalation —
it must never assume passwordless sudo.

**Teardown**: `topctl uninstall` = stop reconciliation → remove ledgered apps
via Dokku → remove TOP storage mounts → remove TOP volumes **only with
`--purge-data`** → remove `/var/lib/top` **only with `--purge-data`** →
optionally `--uninstall-dokku` (separate flag, separate confirmation, prints
what Dokku's own uninstaller leaves behind). Secrets and Git credentials are
retained unless `--purge-secrets`. Every removal logs the identifier and
requires the two-key match above.

## Alternatives considered

- **Assume a dedicated host** — contradicts the brief's homelab reality and
  the ownership-safety goal; rejected as the *only* supported path (still the
  recommended posture in docs).
- **Trust labels alone** — pre-existing containers could coincidentally match
  names; the ledger + label double-check makes destructive ops require two
  independent signals.
- **Fully automated Dokku install without a flag** — violates the approval
  rules for host-level changes; rejected.

## Consequences

- Milestone 2 must implement: inventory snapshot, ledger schema, preflight
  checks, and the gated workflows — before any GitOps work (dependency).
- All approval-gated commands accept `--dry-run` printing the exact plan.
- The e2e suite includes "install onto a host with a pre-existing unrelated
  container, then uninstall, and prove the container survived" (acceptance
  test 11).

## Open questions

- Exact label key prefix (`top.dev/*` placeholder here — must be a namespace
  the project controls or a documented custom prefix; decide in M2).
- Whether `topctl doctor` should offer to *open* required firewall ports or only
  print the ufw/firewalld commands (proposal: print only).

## Amendment (2026-10-31, M3 conformance pass; status unchanged — accepted)

**Reconciler-side consent pair, pinned.** `internal/reconcile` implements
the "`--prune` deletions are gated" decision as an exact consent PAIR:
deletion requires ALL of (1) the `--prune` flag, (2) the `--confirm`
destructive confirmation, and (3) the two-key gate per candidate passing
(owned ledger row ∧ fresh host-snapshot agreement, via
`inventory.Deletable`). Verified semantics worth recording:

- `--prune` WITHOUT `--confirm` is a fail-fast invalid-run error BEFORE the
  lease — nothing is observed, mutated, or recorded.
- The gate refuses per candidate (never a bare run failure): the refusal
  plus the inventory reason string lands in `RunResult.PruneRefusals`,
  mirroring uninstall's skip-with-warning rule.
- `core` namespace services (and the controller's own ref) are never prune
  candidates at all.
- A deleted app drops its ledger row only after a successful destroy.
