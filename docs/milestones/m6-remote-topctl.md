# Milestone 6 — Remote `topctl` operation

(Status: accepted 2026-11-07 with ADR-0016; Tasks T-601..T-604 below are
unscheduled post-v0 work — see backlog.)

## Objective & scope

The operator drives the homelab host from their own machine: every
steady-state verb (`diff`, `doctor`, `deploy`, `history`, `logs`,
`reconcile --now`, `uninstall`) works identically with `--host USER@HOST`
via plain ssh — no Go toolchain on the host, no agent, no new listening
port. `install` stays host-local by design.

## Non-goals

- A TOP agent/daemon on the host (`ADR-0016` rejected-alternative).
- Controller-hosted operator API (depends on TOP being alive; ADR-0005
  keeps the operator channel independent of TOP's health).
- Multi-host fan-out orchestration (composable in shell; post-v0).
- Any secret-value transport — secrets stay on the host (ADR-0008).

## Prerequisites

- v0 shipped (the homelab install that motivated this).
- ADR-0016 owner approval AND Open questions OQ-1/OQ-2 responded to (they
  shape T-601's target grammar directly).

## Deliverables

- `internal/exec` ssh-transport `Runner` variant + tests (BatchMode,
  timeouts, host-key refusal with typed error, redaction parity with the
  exec canon).
- Verbs thread `--host`/`TOPCTL_HOST` through the existing seams; local
  mode is unchanged and remains CI's mode.
- Docs: remote-mode sections in `docs/operations/installation.md`
  ("install runs ON the host") + `docs/contracts/cli.md` `--host` rows +
  an ssh transport note in the platform-backend contract, dry-run
  discipline unchanged.
- Tier-3 sshd testcontainer twin for one representative verb path
  (ssh → real dokku container → assert on the host side).

## Acceptance

1. From a machine that has never met the host, `topctl --host … diff` and
   `topctl --host … history -o json` behave byte-identically (modulo
   transport latency) to the on-host equivalents.
2. Unknown host key → typed refusal with remediation, NOT a merged prompt
   (and no BatchMode hang); explicit `--dangerous-accept-new-host` works and
   is loud.
3. Deploy of self (controller) over this channel keeps its confirm gate
   (ADR-0005) and the old-release-preservation echo.
4. Destructive verbs over ssh obey ADR-0009's consent pair unchanged —
   remote mode adds zero extra capability without a flag.
5. Secret values never transit the operator's machine: proven by an ssh
   capture of a redeploy over the channel (only the secret-annealed
   artifacts do, per existing backend contract).
6. tier-3 ssh-container twin green; the local e2e regression suites stay
   untouched.

## Failure scenarios

- Host unreachable mid-deploy: the whole run fails loud with the ssh exit
  taxonomy reconciled against the exit-code contract (exit 4 = backend
  unavailability — an ssh failure IS a backend unavailability; document the
  mapping).
- Version skew if OQ-2(b): typed mismatch error naming both binaries'
  version strings.
- Operator's stderr confusion between TOP-side and remote-side diagnostics:
  prefix discipline (same convention the adapter uses for dokku stderr).

## Tasks (post-approval, backlog additions)

- **T-601 ssh transport in `internal/exec`** (Runner variant, flags,
  redaction, refusal taxonomy, tier-1 tests).
- **T-602 verb threading** (all steady-state verbs + `--host` grammar +
  docs; destructive gates inherit ADR-0009 verbatim).
- **T-603 tier-3 ssh-container twin** (sshd container + real verbs).
- **T-604 owner docs pass** (homelab install + remote ops runbook updating).

Agent split on approval: core-go (T-601), core-go/cli-go (T-602),
test (T-603), docs (T-604).
