# How-to: reconcile, diff, and inspect operations

For operators driving TOP's reconciliation loop by hand. All flags shown
are copied from `topctl --help` at build time (M3, 2026-10-31); examples
use the fictional platform repo `git@github.com:example/top-platform.git`.

## `topctl diff` — read-only preview

```
topctl diff [--revision SHA] [-o json|text]
```

Zero side effects by construction (the same plan phases as a run, minus
every mutating stage — no lease, no record). Exit codes:

- `0` — converged: no create/update/delete/self-drift rows, no orphans;
- `1` — drift present (the script-friendly anchor);
- `2` — usage error; `4` — git/store wiring unavailable.

Pin a revision instead of the branch HEAD:

```console
$ topctl diff --revision a1b2c3d4
```

## `topctl reconcile` — a full run

```
topctl reconcile [--now] [--service ns/name] [--prune] [--include-controller] [--dry-run] [--confirm]
```

One full pipeline: validate → lease → order → diff → apply → readiness
gate → record → prune. Key flags:

- `--service ns/name` — narrow the run to that service plus its transitive
  dependencies.
- `--prune` — delete services observed on the host but absent from Git.
  Destructive, so it is part of a **consent pair** (ADR-0009): `--prune`
  alone is refused before anything happens; `--prune --confirm` is the
  deterministic scripted path (on a TTY, omitting `--confirm` gives the
  interactive prompt). Deletions additionally require the two-key
  ownership gate per candidate; refusals are reported with reasons, never
  fail the run. Core-namespace services (and the controller's own app) are
  never prune candidates.
- `--include-controller` — include `core/top-controller` in the run
  (confirm-gated). Today it is **report-only**: drift on it is surfaced
  with a change list, never auto-applied (ADR-0005 self-management is M4).
- `--now` — accepted for symmetry with the contract; a manual CLI run is
  already immediate (backoff belongs to the controller loop).
- `--dry-run` — print the plan, exit 0.

Exit codes: `0` converged · `2` usage/gate-unmet · `3` operation failed,
`2` usage/gate-unmet · `3` operation failed, including a lost lease
(envelope code `lease-conflict` — you raced the controller or another
`topctl reconcile`; retry when free) and per-service contained failures ·
`4` git/store/backend unavailable (e.g. no state store yet).

While a controller runs, the SQLite lease serializes runs (ADR-0003/0007):
one writer at a time, other runs exit `3` with `lease-conflict`.

## `topctl deploy` — converge one service

```
topctl deploy <ns/name> [--revision SHA] [--dry-run]
```

Not confirm-gated — it converges toward Git (ADR-0002); the destructive
boundary is the prune consent pair above.

## `topctl history` — operation records

```
topctl history [ns/name] [--limit N]
```

Append-only operation ledger, newest first (default limit 20). Failed rows
carry a redaction-safe hint: `topctl logs <ns/name>` plus the service's
last known-good. Absent state (no store) is an honest empty list, exit 0.

## `topctl logs` — service logs

```
topctl logs <ns/name> [--failed] [--lines N]
```

Thin, redacted pass-through: runtime logs by default, `--failed` for the
last failed deploy's output; the most recent `--lines` (default 100) are
kept.

## `topctl controller run` — the daemon

```
topctl controller run
```

Usually run by Dokku, not by hand. It mounts `POST /webhooks/github`
(HMAC-verified, delivery-deduped), polls the platform repo every 60 s
(missed-webhook backstop), and does a periodic full reconcile hourly;
all triggers coalesce into one serialized run queue. `/healthz` flips to
503-while-draining before the socket closes, so load balancers stop
sending deliveries during shutdown. Configuration keys
(`controller.poll_interval_seconds`, `controller.reconcile_interval_seconds`,
`git.branch`) come from `top-config.yaml` (see `--config`).
