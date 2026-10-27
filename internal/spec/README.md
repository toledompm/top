# internal/spec

Parses and validates **ServiceSpec** YAML files from a platform Git repo and
produces the resolved `domain.Service` values every other package consumes
(docs/contracts/service-spec.md, ADR-0004). This package is the only place
YAML is decoded and defaults are applied.

## File layout requirements

- Repo layout: `<namespace>/<name>.yaml` — one service per file, one YAML
  document per file (multi-document files are rejected).
- A **root-level** file (directly inside the load dir) is allowed only when
  it carries an explicit `metadata.namespace`.
- A file in `<ns>/…` may also state `metadata.namespace`, but it must match
  the directory (layout mismatch = error).
- Dot-prefixed directories/files and symlinks are skipped (never followed).
- `*.yaml` and `*.yml` are read; everything else is ignored.

## Exported API

| Symbol | Purpose |
| --- | --- |
| `Document` / `Metadata` / `Spec` / `Source` / `Port` / `Routing` / `Health` / `Storage` / `Rollout` / `Resources` | YAML-facing types mirroring the contract's field table verbatim |
| `APIVersion`, `KindService` | the only accepted header values (`top/v1alpha1`, `Service`) |
| `LoadDir(dir string, opts ...Option) (Result, error)` | walk + parse + validate a whole repo tree |
| `Result{Services []domain.Service, Warnings []Warning}` | resolved output (services sorted by namespace tier → namespace → name) |
| `WithPlatformRepo(bool)` | marks the repo as the platform repo |
| `ParseError{Path, Line, Err}` | one validation failure with `file:line` |
| `Warning{Path, Field, Code, Message}` + `WarningCode` | non-fatal findings |

## LoadDir behavior and defaults applied

| Default | Value |
| --- | --- |
| `metadata.namespace` (root-level omitted) | none — required for root-level files; nested files take the parent dir name |
| `spec.routing.mode` | `none` |
| `spec.rollout.maxUnavailable` | `0` (zero-downtime checks must pass) |
| `spec.restartPolicy` | `always` |
| `spec.env` | `{}` |
| everything else | absent (no implicit defaults) |

Note: unknown routing modes and restart policies fail; there is no silent
fallback to the default.

## Error taxonomy

- All errors from one `LoadDir` run are aggregated with `errors.Join`; every
  error is a `*ParseError` rendering `"<file>:<line>: <message>"` (line `0`
  when the location is unknown) and unwraps to `domain.ErrInvalid` (or a more
  specific domain sentinel where applicable).
- **Decode errors** — unknown fields (`KnownFields(true)`: typos fail loudly),
  duplicate YAML keys, malformed YAML — each carry the offending line from the
  decoder (`yaml.TypeError` messages are split into one `ParseError` per line).
- **Semantic errors** (`ParseError.Err` text carries the field path):
  - header: wrong `apiVersion`/`kind`, missing `metadata.name`
  - identity: bad namespace/name syntax, `<ns>-<name>` longer than the 48-char
    app budget (via `domain.AppName`)
  - namespace: dir/metadata mismatch, root-level file without namespace,
    invalid namespace, **reserved namespace (`core`, `observability`) used by
    a non-platform repo**
  - source: image XOR build; tag XOR digest; digest must be
    `sha256:<64 hex>`; build requires `repo` + `ref`; tag/digest only valid
    with an image source
  - routing: unknown mode; `proxy` requires non-empty `domains`
  - ports: range, name syntax, duplicates
  - healthcheck: present-but-empty is an error (omit the key for Dokku
    defaults); `http.path` must start with `/`; `tcp`/`http` port references
    must match a declared `spec.ports` entry (name or containerPort);
    negative `intervalSeconds`
  - storage: `name` required, `mountPath` must be absolute, size must be a
    plain quantity, duplicate names
  - env/secretRefs: env keys `[A-Za-z_][A-Za-z0-9_]*`; secretRef names use
    service-name syntax; secret values never appear in YAML (ADR-0008)
  - dependsOn: `ns/name` or bare `name` (same namespace); **cross-namespace
    dependsOn is rejected** (ADR-0004: intra-namespace ordering hints; tier
    ordering is separate)
  - rollout/resources: `maxUnavailable >= 0`; cpu `"2"`/`"500m"`; memory
    `"512Mi"`/`"2Gi"`
- **LoadDir-level**: duplicate identity (same namespace+name across files),
  unreadable files, missing/not-a-directory load dir.

## Warnings (returned even when the run errors)

| Code | Condition |
| --- | --- |
| `mutable-tag` | image tag without a digest; image with no tag/digest at all (implicit latest) |
| `latest-tag` | tag is exactly `latest` |
| `mutable-ref` | build ref does not look like a pinned git sha |

Mutable references stay *allowed* (contract: warned, not rejected).

## Tests

`go test ./internal/spec/... -race` — table-driven over every failure class
above, the contract's illustrative example (testdata/valid, fictional
digests), warnings, defaults, root-level files, duplicate identity,
multi-document rejection, symlink/dot-file skipping, and the
platform-repo/reserved-namespace hook. Unknown-field errors are asserted to
carry their `file:line`.
