# Architecture Decision Records

One ADR per significant product/architecture decision. ADRs are **portable**:
they record product-level decisions and rationale, never private facts about
any contributor's infrastructure (see ADR-0014).

## Format

```markdown
# NNNN — Title

- Status: proposed | accepted | superseded by NNNN | rejected
- Date: YYYY-MM-DD
- Deciders: owner (+ advising agents)
- Needs explicit approval: yes|no

## Context
## Decision
## Alternatives considered
## Rationale and trade-offs
## Consequences
## Open questions
```

Keep them short: readable in under five minutes, detailed enough that a future
contributor understands *why* without asking anyone.

## Index

| ADR | Title | Status | Needs approval |
| --- | --- | --- | --- |
| [0001](0001-product-boundary-dokku.md) | Product boundary: TOP vs Dokku | accepted | yes |
| [0002](0002-git-source-of-truth.md) | Git as source of truth; reconciliation model | accepted | yes |
| [0003](0003-git-sync-webhooks-polling.md) | Git sync: webhooks + polling + retries | accepted | no |
| [0004](0004-servicespec-and-namespaces.md) | ServiceSpec shape; namespace semantics | accepted | yes |
| [0005](0005-bootstrap-self-management-recovery.md) | Bootstrap, self-management, revival | accepted | yes |
| [0006](0006-go-architecture.md) | Go module/package architecture | accepted | no |
| [0007](0007-sqlite-state.md) | SQLite state, migrations | accepted | no |
| [0008](0008-secrets-and-git-access.md) | Secrets handling; private Git access | accepted | yes |
| [0009](0009-host-ownership-safeguards.md) | Host ownership, install/teardown safeguards | accepted | yes |
| [0010](0010-monitoring-architecture.md) | Monitoring stack and its independence | accepted | no |
| [0011](0011-cli-and-mcp-scope.md) | CLI surface; MCP deferred | accepted | yes |
| [0012](0012-testing-strategy.md) | Testing strategy & e2e environment | accepted | yes |
| [0013](0013-backup-scope-boundary.md) | Backup scope: operator-managed, metadata vs application data | accepted | no |
| [0014](0014-public-local-boundary.md) | Public repo vs local environment boundary | accepted | no |
| [0015](0015-bend-laws.md) | Bend formal laws for the pure core (prototype) | accepted | — |
