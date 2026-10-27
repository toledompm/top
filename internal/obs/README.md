# internal/obs

Observability plumbing for TOP (ADR-0006): slog setup with runtime level
toggles and correlation, plus the ADR-0010 Prometheus metric set against a
**private registry**. The package never owns a listener — the controller
server registers the handlers — and never inspects or redacts the strings it
is given (secret redaction is internal/exec's job).

## Logging

```go
log, err := obs.New(os.Getenv("TOP_LOG_LEVEL")) // "debug|info|warn|error", default info
log.SetLevel("debug")                           // runtime toggle, no restart

ctx := log.Ctx(parent, obs.OpInfo{              // carries correlation…
    ID: "op_01J…", Service: "core/top-controller",
    Actor: "webhook", DeliveryID: "gh-…",
})
obs.AppCtxLogger(ctx).Info("applied", "svc", s) // …emitted as an "op" group
info, ok := obs.OpFromContext(ctx)              // identity retrievable directly
```

Every record is JSON on stderr from the handler (`time`, `level`, `msg` plus
the caller's attrs); correlated records additionally carry an `op` group
containing only the non-empty correlation fields.

## Metrics

Built once at controller startup:

```go
m, err := obs.NewMetrics(version, commit) // sets top_build_info once
m.ServeMetrics(mux)                       // /metrics and /healthz on the controller's mux
h := m.HTTPHandler()                      // handler-only use (tests, custom setups)
```

`result` label values are the constants `obs.ResultOK`/`obs.ResultDrift`/
`obs.ResultError`. `service` is the operator-bounded service ref `"ns/name"`.

### Cardinality discipline (ADR-0010)

No git shas, error strings, URLs, delivery ids, or repo URLs in labels —
ever. Label values that are not service refs come from bounded enumerations
(`result`, `event` per the webhook contract, `operation` per the StateStore
contract). `TestLabelSetsBoundedByEnumerations` pins each family's exact
label set so a high-cardinality label cannot be added silently.

### Metric set

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `top_reconcile_duration_seconds` | histogram | — | Whole reconcile run (fetch→apply→record). |
| `top_reconcile_total` | counter | `result` | Reconcile runs by outcome. |
| `top_service_drift` | gauge | `service` | 1 when a service is drifted, 0 in sync. |
| `top_deploy_total` | counter | `service`, `result` | Per-service deploys by outcome. |
| `top_deploy_duration_seconds` | histogram | `service` | Per-service apply + readiness gate. |
| `top_last_successful_reconcile_timestamp` | gauge | — | Unix ts of last fully green run. |
| `top_webhook_deliveries_total` | counter | `event`, `result` | Deliveries by event type and outcome. |
| `top_git_poll_duration_seconds` | histogram | — | One poll cycle against the Git source. |
| `top_git_poll_total` | counter | `result` | Poll cycles by outcome. |
| `top_sqlite_operation_duration_seconds` | histogram | `operation` | State-store latency by op kind. |
| `top_backup_last_success_timestamp` | gauge | — | Unix ts of last successful backup. |
| `top_backup_last_verify_result` | gauge | — | Last verification: 1 ok, 0 failed. |
| `top_build_info` | gauge | `version`, `commit` | Build metadata; always 1. |

Plus the Go and process collectors on the private registry (process metrics
go inert on hosts without `/proc`, e.g. darwin dev machines — never an error).

## Testing

`go test ./internal/obs` is hermetic and portable: log records are decoded
JSON from a captured buffer; metrics are scraped through `httptest`. Note
the module's `go.mod` currently needs `go mod tidy` (sums for
`modernc.org/sqlite`'s transitive deps, added by `tools/tools.go`); run
tests after a tidy.
