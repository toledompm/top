# internal/inventory

The ownership inventory and resource-ledger bridge (ADR-0009). Two jobs:

1. **Snapshot** — a read-only capture of the host state ownership decisions
   need: Docker containers/networks/volumes, TCP listeners, Dokku apps.
2. **Ledger semantics** — connect that snapshot to the SQLite resource
   ledger (`store.Resource`, docs/contracts/state-store.md): bootstrap
   recording of pre-existing resources, the adoption rule, and the
   **two-key deletion rule** — as decisions only, never as deletion
   execution.

Everything reads host state through the `internal/exec` Runner (tests use
`exectest.FakeRunner`); nothing in this package mutates the host. All
commands it runs are read-only observations (ADR-0001 interplay rules:
read-only `docker` is legitimate; mutations belong to the backend adapter
later, never here).

## What it captures

| Section | Command | Notes |
| --- | --- | --- |
| `containers` | `docker ps -a --format json` | newline-delimited JSON objects; `Labels` arrives comma-joined (`"k=v,k2=v2"`), `Names` with a leading slash |
| `networks` | `docker network ls --format json` | |
| `volumes` | `docker volume ls --format json` | |
| `listeners` | `ss -tlnH` | see fragility note below |
| `dokkuApps` | `dokku apps:list --format json` | runs the CLI directly: on first install no backend adapter exists, and at teardown the store must be fed from live reads, not controller state |

`docker`'s `--format json` shorthand is CLI-documented (verified against
`docker ps --help`; the equivalent `--format '{{json .}}'` template is the
same output). Sections degrade **independently**: a missing binary,
failing command, or unparseable line becomes a `Finding` (stable reason
tag + detail); the rest of the snapshot still lands. Unknown is flagged,
never invented — an absent tool means that section is empty with a finding.

Serialization: `HostInventory.JSON()` / plain JSON round-trip; snapshots
are pure data (timestamps UTC), safe to persist under `/var/lib/top/`.

### Host identification

`HostInventory.HostID` is the first 16 hex bytes of SHA-256 over the
host's `/etc/machine-id` content. The machine-id is not a secret, but the
hash keeps upstream ledger/exports keyed by something TOP owns instead of
an upstream per-host identifier. Unreadable/empty source → empty HostID +
a Finding; tests inject the seed via `SnapshotOptions.MachineIDSeed` so
nothing touches the local `/etc`.

## The three identification layers (ADR-0009)

1. **Naming** — TOP-created Dokku apps are `<namespace>-<name>`; the set
   of namespaces comes from `internal/config` (reserved: `core`,
   `observability`, plus `default` policy order). The controller's own app
   is `core-top-controller` (covered by the `core` prefix).
2. **Labels** — Docker resources TOP creates carry `top.dev/managed=true`,
   `top.dev/namespace`, `top.dev/service`. TOP never applies `dokku` or
   `com.dokku.*` labels (ADR-0001 hard rule 2 — Dokku's GC would eat them).
   Unlabeled-but-named matches are adopted only via the bootstrap
   inventory, never silently.
3. **Ledger** — the SQLite `resources` table records every resource TOP
   created (kind, identifier, labels, creating operation, state).

## API

```go
snap, err := inventory.Snapshot(ctx, runner, inventory.SnapshotOptions{
    MachineIDSeed: "", // empty → read /etc/machine-id
    Namespaces:    nil, // empty → config.NamespacePolicyOrder
})

// Bootstrap recording (state: pre-existing), best-effort per row:
report, inserted, err := inventory.CreateLedgerFromSnapshot(
    ctx, st, snap, nil, "op-bootstrap-1", time.Now())

// What TOP's bootstrap created right now (state: bootstrap-owned):
err = inventory.MarkCreated(ctx, st, "app", "core-top-controller", opID, labels, now)

// Decisions (never mutations):
d := inventory.Adoptable(kind, id, snap, namespaces)      // layer1/2 match ∧ in snapshot
d, err := inventory.Deletable(ctx, st, kind, id, freshSnap, namespaces)
```

Decisions carry a typed `Reason` (`ok`, `refused-pre-existing`,
`refused-ledger-missing`, `refused-label-mismatch`,
`absent-from-snapshot`, `refused-foreign-identifier`) and a redaction-safe
`Detail` — the uninstall harness (task B-1) prints skip-with-reason
instead of guessing.

## The two-key deletion rule — truth table

`Deletable` requires **key 1** (ledger: row present, state `managed` or
`bootstrap-owned`) AND **key 2** (fresh snapshot taken at teardown time:
resource present and identified by label or naming). Both independent
keys must agree.

| Ledger row | Fresh snapshot says | Decision | Reason |
| --- | --- | --- | --- |
| missing | anything | refuse | `refused-ledger-missing` |
| `pre-existing` | present + identified | refuse | `refused-pre-existing` |
| `managed`/`bootstrap-owned` | identifier not found at all | refuse (nothing to delete) | `absent-from-snapshot` |
| `managed`/`bootstrap-owned` | present, but `top.dev/managed` label gone / name no longer matches | refuse | `refused-label-mismatch` |
| `managed`/`bootstrap-owned` | present + label or naming agreement | **allow** | `ok` |

Order of evaluation: ledger state first (key 1), then presence, then
identification (key 2). A resource absent from the snapshot but
ledger-managed yields `absent-from-snapshot` — the harness records it and
may mark the ledger, but performs no host action.

`Adoptable` (bootstrap side) differs: no ledger consultation —
TOP-prefix/label match ∧ present in the snapshot. Adoption is explicit:
rows land via `CreateLedgerFromSnapshot` as `pre-existing`, then flow
through the store's `MarkManaged` handoff (which refuses pre-existing
rows, so silent adoption is structurally impossible from the store side).

## Why deletion EXECUTION is not here

This package only decides. Destructive execution (stopping containers,
`dokku apps:destroy`, volume removal, `/var/lib/top` teardown) is the
gated uninstall task (backlog B-1 / phases in `topctl uninstall`,
installer contract). Keeping decides/does separate means the refusal
harness is testable purely with fakes and can never mutate a host by
accident. Callers combine: `Deletable` → gated execution → ledger row
removed via `store.RemoveLedgerEntry` only after success.

## Portability and fragility notes

- `ss -tlnH` (iproute2) is the listener source; the headerless `-H` flag
  exists since iproute2 4.5. Hosts without `ss` (or non-Linux dev hosts and
  macOS) degrade: `ss-missing` Finding, empty listeners, ownership checks
  proceed without the port key. The parse is deliberately shallow
  (`LISTEN` rows, column 3 = local address); malformed rows become
  Findings. **Do not** grow this into a service-discovery engine — ports
  here are a teardown safety key (free 80/443 is doctor's job).
- `docker ps` label values containing commas are mangled by docker
  itself; decisions hinge on top.dev key *presence*, so acceptable.
- `dokku apps:list --format json` has shipped both array-of-strings and
  other shapes across releases; the parser tolerates both, else a Finding.
- Linux/macOS: everything is argv-based through exec, no shebangs, no
  absolute paths — CI-portable.

## Testing tiers (ADR-0012)

- **Tier 1 (default, hermetic):** everything here. `FakeRunner`-canned
  outputs + committed fixtures under `testdata/` (fictional apps:
  `core-top-controller`, `default-website`, `redis-cache`); a temp-file
  SQLite store backs the ledger-bridge tests. Covers: snapshot round-trip,
  independent section degradation, adoption truth table, two-key refusal
  cases with typed reasons, listener skip fallback, naming edge rules.
- **Tier 3 (disposable test host):** a real `docker`/`dokku` host —
  e.g. acceptance test 11 (install over a pre-existing unrelated
  container, uninstall, prove it survived). Not runnable in unit CI.
