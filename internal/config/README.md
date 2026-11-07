# internal/config

TOP's own versioned runtime configuration (`top-config.yaml` on the
controller host). Strict per the ServiceSpec discipline: unknown fields and
a mismatched `apiVersion` are hard errors — typos must fail loudly.

## Load order

`CLI flags (via cfg field assignment after Load) > TOP_* env > file > defaults`.
Use `config.Load(path)` (path may be `""` for defaults+env) and override
inner fields from parsed CLI flags afterwards.

## API

- `Default() Config` — fully-defaulted, valid config.
- `Load(path string) (Config, error)` — strict YAML (`KnownFields`), then
  `TOP_*` env overlay, then `Validate`. `apiVersion` must equal
  `top/v1alpha1` (`config.ErrAPIVersion`). Unknown field →
  `config.ErrUnknownField`.
- `Validate(cfg Config) error` — `*ValidationError` collecting typed
  `FieldError{Field, Err}` entries with dotted JSON/YAML field paths.
- `Config.SecretsDir()` — `<dataDir>/secrets`, the hostfile store directory.

## Environment overrides

Only scalar leaves: `TOP_CONTROLLER_POLL_INTERVAL_SECONDS`,
`TOP_CONTROLLER_RECONCILE_INTERVAL_SECONDS`, `TOP_GIT_URL`,
`TOP_GIT_BRANCH`, `TOP_DATA_DIR` (also `TOP_DATADIR`),
`TOP_SECRETSPROVIDER_KIND`, `TOP_LOG_LEVEL`.

## Field reference (`top-config.yaml`, `apiVersion: top/v1alpha1`)

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `apiVersion` | string | `top/v1alpha1` | required; mismatch → error |
| `controller.pollIntervalSeconds` | int | `60` | Git poll cadence (ADR-0003) |
| `controller.reconcileIntervalSeconds` | int | `3600` | periodic safety reconcile (ADR-0002) |
| `git.url` | string | `""` | platform repo remote (HTTPS+token or SSH) |
| `git.branch` | string | `main` | platform repo branch |
| `namespaces.reserved` | list | `[core, observability]` | platform-only namespaces; `core`/`observability` are immutable members and may not be un-reserved |
| `namespaces.order` | list | `[core, observability, default]` | fixed reconcile order |
| `secretsProvider.kind` | enum | `hostfile` | `hostfile` in v0; `vault` is known-but-unimplemented (accepted by config, fails as `secrets.ErrUnimplemented` at resolution — ADR-0008) |
| `dataDir` | string | `/var/lib/top` | absolute path; overrideable for tests and tier-3 CI |
| `logLevel` | enum | `info` | `debug` \| `info` \| `warn` \| `error` |

## Validation rules

- All interval/retention/schedule fields `> 0`.
- `dataDir` absolute.
- `namespaces.reserved`: non-empty, no duplicates, contains `core` and
  `observability`.
- `namespaces.order`: exactly `core → observability → default`.
- JSON marshal is defined and value-free (CLI `-o json` friendliness).
