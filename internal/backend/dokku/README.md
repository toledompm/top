# internal/backend/dokku

The **Dokku adapter** — the PlatformBackend implementation (T-201 read
slice + T-202 app CRUD) and, per ADR-0001, the ONLY package in TOP that
knows Dokku command syntax. Minimum supported Dokku: **0.38.x** (pinned in
preflight; report flags re-verified against official docs on version
bumps — docs drift is a known hazard, ADR-0001).

## Boundaries (hard constraints, ADR-0001)

- Dokku CLI only, invoked as argv through the `internal/exec` Runner —
  never the Docker API, never out-of-band container mutation of apps.
- Never applies `dokku`/`com.dokku.*` labels to any TOP thing (Dokku's GC
  would eat them); TOP's own labels are `top.dev/*` (ADR-0009).
- Readiness composes from documented reports, never human deploy output.
  (Readiness itself is M3; this slice builds its read primitives.)

## API

`dokku.New(runner exec.Runner) *Client` — stateless Client implementing
`backend.PlatformBackend`. Fields: `Binary` (default `dokku`), `Runner`
(production: `exec.FastExec`), `Log` (nil = discard stderr warnings).

## Command → method mapping (verified Dokku 0.38 docs on dokku.com,
2026-10-27; per-method golden argv pinned in dokku_test.go)

| Method | Dokku argv |
| --- | --- |
| `existsApp` (internal probe) | `apps:exists <app>` — non-zero ⇒ not exist (verified tier-3: canonical not-exist exit is **20**, `!  App <app> does not exist`) |
| `EnsureApp` | `apps:exists` → `apps:create <app>` |
| `Inspect` | `ps:report <app> --format json`; `builds:report <app> --format json`; `git:report <app> --format json`; `apps:report <app> --format json`; `ports:report <app> --ports-map-json`; `storage:list <app> --format json`; `config:keys <app>`; `network:report <app> --format json`; `resource:report <app> --format json` |
| `LastGoodRevision` | `apps:report <app> --app-deploy-source-metadata`, fallback `git:report <app> --git-source-image` |
| `Stop` | `apps:exists` → `ps:stop <app>` |
| `Destroy` | `apps:exists` → `apps:destroy --force <app>` |
| `HealthyBackend` | `dokku version` + `dokku apps:list --format json` |
| `EnsureNetwork` | `network:exists <name>` → `network:create <name>` |

Report → ObservedState mapping notes:

- Report JSON keys follow 0.38's "flag name minus plugin prefix" rule;
  the deprecation-window legacy aliases (`ps-…`, `app-…`) are accepted as
  fallback keys. `git:report` read-only flags are read with both
  spellings (`sha`/`git-sha`) because the docs do not state the stripped
  form for read-only flags.
- `deploy-source=docker-image` (or set `source-image`) ⇒ `Image` = the
  deploy-source metadata; otherwise the metadata (git sha) ⇒ `Revision`.
- `build-status`: `canceled`/`abandoned` map to `BuildFailed` (the last
  build did not succeed; closes over the missing domain enum — see the
  contract's readiness use in M3). `restart-policy: unless-stopped` has
  no domain counterpart and reads as "" (unreported), never guessed.
- `ports-map-json` entries map `scheme → Port.Name`, `container_port →
  ContainerPort`, `host_port → HostPort`.
- `storage:list` entries map `entry_name → StorageMount.Name`
  (0.38's named-entry form; legacy host-path mounts render inside the
  entry name `legacy-<hash>`).
- `config:keys` gives env key NAMES only, one per line — values are never
  read (ADR-0008). `SecretChecksums` stays empty in this slice: computing
  checksums would require reading values; the reconciler keeps checksums
  upstream (store layer), per the M3 plan.
- Networks come from `network:report` computed attach lists
  (space-separated), tokens deduped + sorted.

## Timeout & error classes

| Class | Commands | Timeout | Error class |
| --- | --- | --- | --- |
| read | reports, `apps:exists`, `network:exists`, list/keys/version | 30 s | `ErrBackend` (`*BackendError`, cause `*exec.ExitError`) |
| mutate | `apps:create`, `ps:stop`, `network:create` | 2 min | `ErrBackend` |
| destroy | `apps:destroy --force` | 5 min | `ErrBackend` |
| any/process-level | timeout / binary missing | — | `ErrUnhealthy` (via `BackendError.Unhealthy`) |
| backend-health | `HealthyBackend` both commands | 30 s | `ErrUnhealthy` |

A non-empty stderr with exit 0 is a warning: logged via `Client.Log`,
never a failure (contract error semantics). Redaction of secret values is
the exec layer's job; argv is passed unredacted and no values are logged
here. `BackendError` multi-unwraps to both its cause and the class
sentinel (`errors.Is` on `domain.ErrBackend`/`ErrUnhealthy`, `errors.As`
to `*exec.ExitError` for exit code + redacted stderr).

## Ownership split (ADR-0009) for `Destroy`

The destructive gate — ledger match AND `top.dev/*` label/naming two-key
match — is proven and logged by the CALLER (uninstall/prune, T-204/T-208).
This adapter's defense-in-depth is the naming assertion only: it refuses
one step earlier.

## Deviations / open items

- `Inspect` does **not** populate `Domains` — no domains-report flag was
  verified this wave (M3 adds `domains:report` once its JSON keys are
  confirmed on 0.38).
- `apps:report --app-locked` is read but `ObservedState` has no lock
  field yet (lands with M3 `Apply`, which needs it first).
- Deploy-orchestrating `Apply`/`Readiness`/`FailedLogs` are M3 interface
  extensions.
- `apps:exists` not-exist exit code: docs say only "non-zero"; tier-3
  verified **20** as the 0.38.32 canonical answer (`!  App <app> does not
  exist`); 10/1 accepted as loose aliases, other codes surface as infra
  failures.
- `resource:report --format json` is relied on although the resource docs
  page shows flag-form output only (the standard report skeleton provides
  `--format json`; e2e verifies).
- Verified live on 0.38.32 (tier-3 run 2026-10-27): report `--format
  json` returns a single flat object carrying BOTH stripped and
  legacy-prefixed keys; a never-deployed app's `git:report` reports
  `"sha":"HEAD"` — and `Revision` is therefore only set when
  `domain.ValidRevision` accepts the value (fresh-repo "HEAD" reads "");
  accepted quirk: `apps:create` on a mid-bootstrap fresh container can
  transiently exit 1 ("No web listeners specified") — the e2e harness
  gates readiness with an idempotent canary create+destroy and the
  lifecycle test retries EnsureApp (the reconciler's M3 retry absorbs
  production instances).


## Testing tiers (ADR-0012)

1. **Tier 1 — fake-exec golden argv** (default `make test`):
   `dokku_test.go` against `internal/exec/exectest.FakeRunner` — exact
   argv per method, all Inspect composition paths, not-found / locked /
   unhealthy / report-failure / malformed-JSON mapping, invalid-ref
   refusals.
2. **Tier 3 — real Dokku** (`TOP_E2E=1 make test-e2e-backend`):
   `e2etest/` (build tag `e2e`) — provisions the official
   `dokku/dokku` container through testcontainers-go with a docker-sock
   mount, then runs EnsureApp → Inspect (never-deployed mirror) →
   `git:from-image` a tiny public image (`httpd:2.4-alpine`) → Inspect
   (deployed+running) → Stop/Destroy idempotency → HealthyBackend.

**Never point the tier-3 suite at a host Dokku** — it is wired to its own
container. Apps touched are `top-` prefixed regardless.

Cost: first run pulls `dokku/dokku` (~1 GB); each run creates and tears
down a disposable container on the local Docker daemon (macOS Docker
Desktop supported).
