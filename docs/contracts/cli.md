# Contract: CLI surface (`topctl`)

cobra, one binary named **`topctl`** (never `top` — that collides with the
Unix process viewer; see ADR-0011), agent-consumable. Global flags:
`-o json|text` (default text), `--schema-version` negotiation reserved,
`--redact=on` (default). JSON outputs are versioned objects
(`"schemaVersion": 1`).

## Command → contract mapping

| Command | Type | Uses | Notes |
| --- | --- | --- | --- |
| `topctl doctor` | read-only | Installer phase 0 | exits non-zero with per-check failures + fixes |
| `topctl install <repo> [--install-dokku] [--dry-run]` | mutating, gated | Installer | approval prompts per ADR-0009 |
| `topctl status [--namespace N] [--watch]` | read-only | Backend.Inspect + Store | per service: revision, state, health, last op |
| `topctl diff [--revision R]` | read-only | Reconciler `DiffOnly` (T-309) | **zero side effects** — plan phases minus every mutating stage (no lease, no record); exit 1 on any create/update/delete/self-drift row or orphan, 0 clean; `--revision` pins a git sha instead of HEAD |
| `topctl reconcile [--now] [--service ns/name] [--prune] [--include-controller]` | mutating | Reconciler.Run | `--prune` and `--include-controller` require the confirm gate (untouched non-TTY exits 2); run failures, contained per-service failures and lease conflicts (envelope code `lease-conflict`) are exit 3; git/backing unavailability 4; `--dry-run` prints the plan, exit 0; `--service` narrows to that service + its transitive dependencies |
| `topctl deploy <ns/name> [--revision sha]` | mutating | Reconciler (single service) | converge the named service + its closed dependsOn set through a full run; not confirm-gated for user apps (converges toward git per ADR-0002 — the destructive boundary stays the prune consent pair); **exception: `core/top-controller` IS confirm-gated** (ADR-0005 human gate for self-management; on failure the old release stays in place; amendment R4, blessed wording pending owner verdict) ; `--dry-run` identical shape |
| `topctl history [ns/name] [--limit N]` | read-only | Store | operation records newest first (limit default 20); failed rows render the `topctl logs <ns/name>` + last-known-good hint vocabulary; absent state = empty list, exit 0 |
| `topctl logs <ns/name> [--failed] [--lines N]` | read-only | Backend.ServiceLogs / FailedLogs | thin, redacted pass-through: runtime `logs <app> [--num N]`, `--failed` → `logs:failed`; most-recent `lines` kept (default 100) |
| `topctl backup create|list|verify` | mixed | BackupProvider | create is mutating but non-destructive |
| `topctl recover diagnose|restart|redeploy|restore` | mixed | Recovery | **works with controller down** (ADR-0005) |
| `topctl uninstall [--purge-data] [--purge-secrets] [--uninstall-dokku] [--dry-run]` | destructive, gated | Installer teardown | tiered confirmations |
| `topctl secrets set <name> [--overwrite]` / `list` | mixed | secrets.Provider | set reads the value from stdin (never argv/shell history); list manifests names+checksums only (ADR-0008/0013); wired M2 |
| `topctl controller run` | daemon | Controller loops + HTTP | usually run by Dokku, not by hand |
| `topctl version` | read-only | — | includes build info + min Dokku version |

## Safety invariants (tested)

- Read-only commands make **no** host mutations (no lock acquisition, no
  writes except local caches explicitly documented).
- Every mutating command supports `--dry-run` printing the plan.
- Every approval-gated command supports `--confirm` for a deterministic
  non-interactive path (agents/CI tear-down-and-reinstall cycles are a
  first-class use case, ADR-0009); gated destructive actions always also
  require their specific flag (`--purge-data`, `--prune`, …) — there is no
  single "yes to everything" switch.
- Secret values never appear in any output mode (exec-layer redaction +
  store-level discipline).
- Exit codes: 0 ok; 1 drift/diagnose-findings (for scripting); 2 usage;
  3 operation failed (includes a lost reconcile lease — envelope code
  `lease-conflict`, same row, T-309); 4 backend/store unavailable.

## Illustrative

```console
$ topctl diff -o json
{"schemaVersion":1,"revision":"a1b2c3","services":[
  {"service":"observability/prometheus","change":"update",
   "fields":[{"path":"spec.source.digest","from":"sha256:aaa…","to":"sha256:bbb…"}]}],
 "summary":{"create":0,"update":1,"noop":6,"orphaned":0}}
$ echo $?   # 1 → drift present
```
