# Risk register

Likelihood/Impact: L/M/H. Mitigations reference ADRs and milestones. Owner
review requested for all **H** rows.

| # | Risk | L | I | Mitigation | Residual |
| --- | --- | --- | --- | --- | --- |
| R1 | **Controller self-update bricks the reconciler** → no future reconciliation | M | H | Human-gated self-deploy (ADR-0005); Dokku checks gate the switch; last-known-good recorded; `topctl recover redeploy` is controller-independent | Low |
| R2 | **Uninstall/teardown deletes foreign resources** on a shared host | M | H | Ownership ledger + label double-match + pre-existing inventory snapshot (ADR-0009); e2e "foreign container survives" test; tiered `--purge-*` flags | Low |
| R3 | **Forged/replayed webhooks** trigger unwanted reconciles or leak info | M | M | HMAC-SHA256 verify, delivery dedup, payload never executed, desired state always re-fetched (ADR-0003) | Low |
| R4 | **Git credential compromise** (deploy key on host) | L | M | Read-only key, strict known_hosts pinning, 0600 perms, excluded from backups, rotation runbook (ADR-0008) | Low |
| R5 | **Secret leakage** via logs/SQLite/metrics/CLI output | M | H | Reference-only model, exec-layer redaction, store discipline, tested invariants (ADR-0008, contracts) | Low-Med |
| R6 | **SQLite corruption/loss** | M | M | WAL + busy_timeout + integrity-check'd backups + restore safeguards (ADR-0007); rebuild-from-Git + re-adoption is the floor | Low |
| R7 | **Backup false confidence** (operator thinks app data is covered) | M | H | Loud scope boundary in docs/CLI/release notes (ADR-0013); cookbook for app-level backups | Med (human factor) |
| R8 | **Dokku docs/version drift** breaks the adapter | H | M | Min-version pin (0.38.x), JSON-report-based introspection (not output scraping), tier-3 real-Dokku CI on adapter changes (ADR-0001/0012) | Low-Med |
| R9 | **Fighting Dokku's automation** (event-listener, ps:restore, label GC) | M | M | Hard rules in ADR-0001 (CLI-only, no `dokku` labels, tolerate restore window); tier-3 tests assert coexistence | Low |
| R10 | **Small-host resource exhaustion** (controller + prometheus + grafana + cadvisor) | M | M | Documented minimums, small-host defaults (retention caps), ServiceSpec resource limits, preflight headroom check (ADR-0010/0009) | Med |
| R11 | **Bootstrap gap**: install fails mid-way leaving half-platform | M | M | Phased idempotent installer, ledger markers, `topctl recover diagnose` guidance (installer contract) | Low |
| R12 | **Webhook endpoint unreachable** (no public ingress) | H | L | Poll-only mode is fully supported; webhook is an optimization (ADR-0003) | Low |
| R13 | **Mutable image tags** (`latest`) cause surprise deploys or stale deploys | M | M | Digest recommended, warning in diff, `--force` pull on detected tag move (ADR-0002) | Low-Med |
| R14 | **Operator runs TOP on its only host with critical data** | M | H | Docs steer to dedicated/disposable host; acceptance tests run on disposable VMs; destructive ops gated (ADR-0009/0012) | Med (human factor) |
| R15 | **Single-maintainer sustainability** | M | M | Small scope, one binary, few deps (researched), full docs/ADR trail, e2e runnable by strangers | Med |
