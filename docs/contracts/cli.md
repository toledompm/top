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
| `topctl diff [--revision R]` | read-only | Reconciler plan | **zero side effects**; exit 1 on drift (script-friendly) |
| `topctl reconcile [--now] [--service ns/name] [--prune] [--include-controller]` | mutating | Reconciler | `--prune` and `--include-controller` require confirmation |
| `topctl deploy <ns/name> [--revision sha]` | mutating | Reconciler (single service) | the controller self-update path (ADR-0005) |
| `topctl history [ns/name] [--limit N]` | read-only | Store | operation records (see reconciler.md example) |
| `topctl logs <ns/name> [--failed]` | read-only | Backend | thin, redacted pass-through |
| `topctl backup create|list|verify` | mixed | BackupProvider | create is mutating but non-destructive |
| `topctl recover diagnose|restart|redeploy|restore` | mixed | Recovery | **works with controller down** (ADR-0005) |
| `topctl uninstall [--purge-data] [--purge-secrets] [--uninstall-dokku] [--dry-run]` | destructive, gated | Installer teardown | tiered confirmations |
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
  3 operation failed; 4 backend/store unavailable.

## Illustrative

```console
$ topctl diff -o json
{"schemaVersion":1,"revision":"a1b2c3","services":[
  {"service":"observability/prometheus","change":"update",
   "fields":[{"path":"spec.source.digest","from":"sha256:aaa…","to":"sha256:bbb…"}]}],
 "summary":{"create":0,"update":1,"noop":6,"orphaned":0}}
$ echo $?   # 1 → drift present
```
