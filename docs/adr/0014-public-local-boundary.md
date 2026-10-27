# 0014 — Boundary between public project artifacts and local environments

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no (policy, already owner-mandated)

## Context

TOP is an open-source project developed against a private test host. Without
an explicit, enforced boundary, private hostnames, network details, and
credentials leak into docs, examples, tests, ADRs, and agent-generated
artifacts.

## Decision

1. **Tracked files are portable.** Docs, ADRs, examples, tests, CI, and task
   descriptions must be understandable and runnable by a stranger. Examples
   use reserved/fictional values: `example.com` hostnames,
   `github.com/example/...` repos, no real IPs.
2. **Local material lives in `.local/`**, gitignored at the top level.
   Contents: host inventories, access notes, local env overrides, private
   diagnostics. A committable fictional template lives at
   `examples/local-setup.example.md`.
3. **Generalize, don't sanitize-after-the-fact.** If a local observation
   matters to the product, the *general requirement* is extracted into an ADR
   or architecture note ("must support hosts with ~4 GB RAM"), while the
   observation itself stays in `.local/`.
4. **Secrets never touch the repo, agent memory, logs, or task text.** Local
   secrets stay outside the repo entirely (OS keychain / password manager);
   runtime secrets follow ADR-0008.
5. **Enforcement**: `.gitignore` covers `.local/` and secret patterns; a CI
   hygiene job greps tracked files for forbidden patterns (RFC1918/CGNAT
   literals outside docs about addressing, the owner's known private tokens
   patterns, `BEGIN ... PRIVATE KEY`); PR template includes a disclosure
   checklist. The planning-phase hygiene review (this milestone's
   deliverable) runs the same check manually.

## Alternatives considered

- **Private docs repo + public code repo** — splits knowledge, doubles
  tooling, and the boundary question just moves. Rejected.
- **Rely on review discipline only** — fails silently under agent-assisted
  development; the CI grep is cheap insurance. Rejected as sole mechanism.

## Consequences

- New contributors can follow `docs/operations/installation.md` on a clean VM
  without any private material — this is an explicit acceptance test (M2/M5).
- Agents working on this repo must check `.gitignore` coverage before
  creating files from live diagnostics.

## Open questions

- None; enforcement mechanics may improve over time (e.g. gitleaks in CI).
