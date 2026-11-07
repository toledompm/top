# 0011 — CLI surface (`topctl`); MCP deferred to post-v0

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27 (amended same day per owner review)
- Needs explicit approval: **yes** (for the deferral)

## Context

The product goals include "safe, structured operations suitable for use by
coding agents through a CLI and, potentially, MCP". Two questions: what the
binary is called, and whether an MCP server ships in v0 or later.

## Decision

**The CLI binary is `topctl`, not `top`.** `top(1)` is the ubiquitous Unix
process viewer; shipping a binary named `top` would shadow a standard tool
on every operator's PATH and break muscle memory and scripts. The product
stays "TOP" (project name, `/var/lib/top`, `top-` resource prefixes,
`install-top.sh`); only the command is `topctl` (kubectl-style).
Owner-directed amendment during M0 review.

**v0 ships a CLI designed for agent consumption; MCP is a post-v0 milestone.**

CLI (cobra), one binary, every command supports `-o json` with a stable,
versioned output schema:

- `topctl diff [--revision R]` — desired vs observed, exit code reflects drift
- `topctl reconcile [--now] [--service ns/name]`
- `topctl deploy <ns/name> [--revision <sha>]` (incl. the gated controller
  self-deploy, ADR-0005)
- `topctl history <ns/name>` — deployment/reconciliation operations
- `topctl logs <ns/name>` — convenience proxy to Dokku logs
- `topctl install <repo>` / `topctl uninstall` — approval-gated (ADR-0009)
- `topctl doctor` — portable preflight/environment check usable pre-install

Safety properties for agent use: read-only commands are guaranteed
side-effect-free; mutating commands accept `--dry-run`; destructive commands
require `--confirm` or interactive approval; all output redacts secrets.

**MCP deferral mechanics**: CLI commands are thin wrappers over an internal
application-service layer (`internal/reconcile`, `internal/install`, …), so a
future `topctl mcp serve` can expose the same operations with explicit tool
boundaries (read tools vs write tools vs approval-required tools) without
refactoring. The MCP ADR, when written, must define: tool allowlist, which
operations remain human-only, authn for the MCP endpoint, and audit logging.

## Alternatives considered

- **Ship MCP in v0** — every safe-MCP concern (authz boundaries, accidental
  mutation via tool call, audit trail) duplicates work the CLI must do first;
  it also expands the v0 security surface exactly where agents will drive it.
  Deferred, not rejected.
- **CLI for humans only, pretty output** — would push agents to parse prose;
  structured output from day one is nearly free and is the right foundation.
- **gRPC/REST control API in v0** — unnecessary for one host + one CLI.

## Rationale and trade-offs

The expensive part of "agent-safe operations" is the operation layer
(idempotency, dry-run, confirmation, redaction, structured errors) — all of
which the CLI needs anyway. Sequencing MCP after that layer exists makes MCP
a thin protocol adapter instead of a parallel product.

Trade-off: coding agents must shell out to the CLI rather than call tools
natively in v0. With `-o json` and clean exit codes that is a good interface,
and it is how agents already drive kubectl/flux.

## Consequences

- Milestones 1–5 contain no MCP work; a candidate Milestone 6 is listed in
  `docs/milestones/README.md`.
- Contract docs define the JSON output schema as versioned
  (`schemaVersion` field) so future MCP can reuse it.

## Open questions

- Should `topctl mcp serve` (when built) be stdio-only for local agents, or also
  HTTP? Decide in the future MCP ADR; stdio-only is the safer default.
