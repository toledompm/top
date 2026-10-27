# internal/plan

TOP's **diff/plan computation** — the pure function at the heart of the
reconciliation loop. It turns `(desired, observed)` into a `domain.Plan`
without touching the host, disk, network, or the state store. `topctl diff`
and the reconciler's Apply both run **exactly this computation** (ADR-0002
parity guarantee): applying a plan converges the host; diffing renders it.

Model decisions: ADR-0002 (level-based reconcile, drift-correction scope),
ADR-0005 (controller self-service is report-only), ADR-0008 (secrets are
names/checksums, never values). Output vocabulary:
`internal/domain` (`ServicePlan`, `FieldChange`, `ChangeKind`).

## API

```go
Compute(desired domain.Service, observed domain.ObservedState, opts ...Option) domain.ServicePlan
ComputeAll(desired []domain.Service, observed map[domain.ServiceRef]domain.ObservedState, opts ...Option) domain.Plan

WithPruneAllowed(bool) Option  // default false — prune is never implicit
WithSelfService(domain.ServiceRef) Option // marks the controller service (drifted(self))
```

- `Compute` is pure and errorless (both inputs are already validated
  upstream): absent observed ⇒ `create`; empty diff ⇒ `noop`; otherwise
  `update` (or `drifted(self)` when the service is the controller).
- `ComputeAll` delegates per service; a **missing observed entry means
  "never deployed" ⇒ create**. It also fills `Plan.Orphaned`: observed
  apps absent from Git. Deletes happen only with `WithPruneAllowed` —
  and *never* for `core`-namespace apps or the self-service ref
  (ADR-0004/0005); orphans are reported every time.
- `ComputeAll` leaves `Plan.Revision` to the caller: only the reconciler
  knows the resolved sha at plan time.

## Determinism

- Field changes are sorted by change path; service plans are sorted by
  `ServiceRef` (namespace, then name — lexical; tier/dependsOn ordering is
  the reconciler's ordering step, not the plan's).
- Map iteration order can never leak: env keys, secret refs, sets are all
  sorted before comparing. Same inputs ⇒ byte-identical plan (tested).

## Idempotency invariant (tested)

`Compute(desired, observedShapedExactlyLikeDesired) → noop with no
changes`, for **every** service in `internal/spec/testdata/valid/*.yaml`.
The test synthesizes observed state from a desired `Service` — written
against `docs/contracts/platform-backend.md`'s `Inspect` table, mirroring
what a correct backend reports (env as *keys*, secretRefs as injected key
names + checksums, storage entry names prefixed `<app>-`) — never from
Compute's own internals.

## Coverage matrix (ADR-0002 drift scope vs ObservedState v0)

Legend: ✅ compare (analogous entry kinds per row); ⚠️ partial; — skip.

| Field (ADR-0002 scope) | Entry path | Supported? | Notes |
| --- | --- | --- | --- |
| App existence | kind `create`/`noop` | ✅ | `observed.Absent()` ⇒ create, all fields as adds |
| Revision/image | `revision` | ✅ | image sources: desired `Source.Reference()` vs observed `Image`; source-type switches covered. Source builds: only full 40/64-hex commit shas; **branch-pinned build refs are not comparable** (resolve step's job) |
| Env keys, plain values | `env.<KEY>` | ⚠️ | ObservedState v0 carries **names only** (ADR-0008 redaction: drift detection compares names/checksums, never values). Missing key ⇒ add with the plain desired value. Key present ⇒ assumed converged — *value drift is undetectable v0*; a backend env checksum closes this in M3 |
| Env key removal | — | — | **Not emitted v0.** Dokku env is a sink (ADR-0008): from `(desired, observed)` alone TOP cannot distinguish a manually-set key from the residue of a dropped secretRef, and never silently purges env it did not set. Removal detection needs the *recorded* last-applied state — store-backed reconciler work (M3) |
| Secret refs | `secretRefs.<NAME>` | ⚠️ | Name presence only: absent ⇒ add, observed checksum `""` (unresolvable on host) ⇒ modify (forces re-set). **Rotation** (value changed on host) needs the store's recorded checksum — M3. Entries are name-only: `From`/`To` structurally nil (`domain.NewSecretFieldChange`) |
| Ports | `ports.<name>` (or `ports.<number>`) | ✅ | Symmetric set by name; unnamed observed ports matched by containerPort to avoid add/remove noise. HostPort pinned ⇒ compared too |
| Routing mode | — | ⚠️ | ObservedState v0 has no routing-mode field. `proxy → internal/none` is represented via the **domains** set shrinking to nothing |
| Domains | `domains.<host>` | ✅ | Symmetric set |
| Storage mounts | `storage.<name>:<mountPath>` | ⚠️ | Symmetric by MountPath; entry-name comparison tolerantly strips the `<app>-` prefix. **Size is advisory and not reported** — not comparable |
| Restart policy | `restart-policy` | ✅ | Value equality. Observed empty = "unreported" ⇒ skipped (not comparable, not drift) |
| Resource limits | `resources.cpu`, `resources.memory` | ✅ | Per-quantity string equality; fully-unreported observed `Resources` ⇒ skipped |
| Network attachments | `networks.<name>` | ✅ | Desired derives from routing mode (`internal` ⇒ shared `top-internal` network — the name is hardcoded v0, see below); symmetric set |
| Healthcheck | — | — | **Not in ObservedState v0 — not comparable yet.** M3 adapter extends ObservedState |
| Rollout (MaxUnavailable) | — | — | Not observable via Inspect; apply-time concern |
| dependsOn | — | — | Ordering hint, not host state |

## Controller self-service special case (ADR-0005)

`WithSelfService(ref)` marks TOP's own controller (e.g. `core/top-controller`). Drift on it:

- yields kind **`drifted(self)`** — `ChangeKind.IsDestructive()` is true,
  so every confirmation/dry-run gate that consults `Plan.Destructive()`
  stops on it *before* any mutation;
- still carries its field-level changes — they are the *report* the
  human acts on (pin the drifted sha, or edit the spec);
- must never be auto-applied: no caller honoring the reconciler contract
  derives an apply from a `drifted(self)` plan. When converged, the self
  service plans as an ordinary `noop`.

Orphans whose ref is self-marked or `core` are never given `PlanDelete`
plans even under `--prune` — reports only.

## Secret safety invariants (enforced here, tested)

Every secret-ref/re-secret change is built with
`domain.NewSecretFieldChange`: only the path name and a marker survive;
`From`/`To` are structurally nil, so no value — however the caller slices
or renders the plan — can leak. Plain env adds *do* carry the desired
value, by design: plain env values live in Git.

## Boundaries & tests

- Imports: stdlib + `internal/domain` only. No I/O, no reflection, no
  interfaces.
- Tests (`plan_test.go`): the set-idempotency invariant over all spec
  fixtures, fleet-level invariant, create/add coverage, a drift table
  asserting exact change paths per mutation, determinism (byte-stable,
  sorted), self-service report-only, orphan/prune rules, secret
  no-value-leak, and the build-ref comparability boundary.

## Known deviations & M3 follow-ups

1. **Env value drift undetectable** (names-only observed env) — needs a
   backend env value-checksum in `ObservedState` (M3 adapter).
2. **Env removal detection** needs recorded last-applied state (store
   layer), not `(desired, observed)`; Compute is deliberately
   placeholder-silent here (sink policy).
3. **Secret rotation** detection needs the store's recorded checksums
   upstream of Compute.
4. **`internalNetwork` name is hardcoded** ("top-internal") until the
   backend exposes the configured network; backend and plan must agree.
5. **Healthcheck / mount sizes / routing mode** unobservables listed in
   the matrix above; the M3 `internal/backend/dokku` Inspect adapter
   should extend `ObservedState` rather than special-casing the plan.
6. Unnamed-port matching is a heuristic to keep diff noise down; the
   backend contract should document port-name presence guarantees.
