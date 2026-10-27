# 0008 — Secrets handling and private Git access

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes**

## Context

Workloads need secrets (API keys, tokens) and TOP needs credentials to read
private Git repos. Constraints from the brief: no plaintext secrets in Git or
in ordinary SQLite records, no custom secrets platform, account for file
permissions and backup implications.

## Decision

**v0: a host-local secret store of plain files, referenced by name.**

- Location: `/var/lib/top/secrets/<name>` — one file per secret, mode `0600`,
  owned by the user TOP runs as, directory `0700`. Managed out-of-band
  (operator writes them; `topctl secrets set <name>` helper reads from stdin to
  keep values out of shell history).
- ServiceSpecs reference secrets only by name (`secretRefs`). The reconciler
  resolves names → values at apply time and injects them via
  `dokku config:set --no-restart` batched with the deploy that follows.
- **Storage boundaries**: secret values never enter Git, SQLite (only the
  fact that secret `<name>` was applied at time T), logs, metrics, or CLI
  output (the exec layer redacts registered secret values and anything marked
  sensitive, ADR-0006).
- **Backup implication**: the secret store is deliberately *excluded* from
  metadata backups (ADR-0007) — a backup must be safe to copy off-host.
  Disaster recovery documents that secrets are re-provisioned out-of-band;
  recovery runbooks list *which* secrets exist (names only) so nothing is
  forgotten.
- **Git credentials**: a dedicated **read-only deploy key** for the platform
  repo, stored at `/var/lib/top/git/` (`id_ed25519` mode 0600 + a pinned
  `known_hosts`). All fetching goes through the git CLI with
  `GIT_SSH_COMMAND="ssh -i … -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile=…"`. Host-key verification is never disabled; key
  rotation is a documented manual procedure. HTTPS+token is supported as an
  alternative, token stored the same way (0600 file). The one exception to
  read-only: the `install --init` repo-creation flow (ADR-0005) pushes the
  template's initial commit using a **one-time write-capable credential**
  supplied at the prompt — used for that push only, never stored; the
  steady-state sync key stays read-only.

**Post-v0 direction (owner-directed, M0 review): external secret providers,
not encrypted-in-Git secrets.** The future is integrating providers like
Vault to inject secrets at runtime. To make that an addition rather than a
redesign, v0 defines a **`SecretProvider` interface** behind `secretRefs`:

```text
resolve(ctx, refName) → (value, version)   // version drives rotation
```

v0 ships exactly one implementation (`hostfile`, the plain-file store
above). A later `vault` provider (or cloud secret managers) plugs into the
same interface; ServiceSpecs and the reconciler's resolve-at-apply flow do
not change. Injection model for the provider era: the controller resolves
from the provider at apply time and injects via Dokku env (same sink as
today), with provider-side versioning driving rotation; true runtime
injection (an in-container agent/sidecar that never lands values in Dokku
env) is a further-future variant and gets its own ADR when designed, since
it changes the trust and lifecycle model. SOPS/age-encrypted-secrets-in-Git
was considered and is **rejected by the owner** — not the project's
direction.

## Alternatives considered

- **SOPS/age in Git** — better GitOps purity (secrets versioned with specs)
  but rejected by the owner: the project's future is external providers
  (Vault-style), not encrypted-at-rest-in-Git secrets.
- **Dokku's own env as the secret store** — values would sit in Dokku's
  on-disk env files regardless (that's where they must end up to be
  injected), but treating Dokku as *source of truth* would break the
  desired-state model (Git would no longer fully describe intent). Dokku env
  is a downstream sink only; drift detection compares names/checksums, not
  values.
- **Vault in v0** — a "complex secrets-management product" is explicitly
  out of scope for v0; the `SecretProvider` seam keeps it a pure addition
  later.
- **go-git for fetching** — rejected (its SSH/known_hosts path is in flux);
  git CLI gives exactly OpenSSH semantics. See research notes in
  `docs/architecture/overview.md` appendix.

## Consequences

- `topctl diff` can show *that* a secret will change (name + staleness) but
  never its value.
- Uninstall leaves `/var/lib/top/secrets/` and `/var/lib/top/git/` in place
  unless `--purge-secrets` is typed (ADR-0009).
- Deploy key permissions are read-only: even a compromised controller cannot
  push to the platform repo.

## Open questions

- Should secret *rotation* trigger a rolling restart of referencing services
  (hash-of-names + mtime in desired state)? Proposed: yes, via a `secretVersion`
  annotation the operator bumps; decide in Milestone 3.
