# Tearing down TOP

> Status: **real (M2, re-verified against the code in the T-506 sweep,
> 2026-11-03)** — matches the implemented `topctl uninstall`
> (`internal/install/uninstall.go`) and
> `docs/contracts/installer.md`'s teardown contract, with one documented
> implementation choice: `--uninstall-dokku` **prints** Dokku's purge
> commands instead of auto-running them (below). Ownership and approval
> rules: ADR-0009.

## Principles

- TOP deletes **only** what it can positively identify as its own: the
  resource must be in TOP's persisted ledger *and* match in a fresh host
  snapshot (ledger + label — the two-key rule). Disagreement → skipped with
  a warning and the printed manual command, never guessed.
- Foreign containers, volumes, networks, firewall rules, and proxy config are
  never touched.
- Data is kept by default; every destructive tier is its own flag and nothing
  implies another — there is no single "yes to everything" switch.

## Command (verified flags)

```bash
topctl uninstall --dry-run          # the full decision list, nothing touched
topctl uninstall                    # removes TOP-managed apps + mounts
topctl uninstall --purge-data       # + storage entries, <dataDir>
topctl uninstall --purge-secrets    # + secrets and git key material
topctl uninstall --uninstall-dokku  # + Dokku itself (prints its manual runbook; see below)
```

Non-interactive use (agents, CI, e2e): add the global `--confirm` — the
destructive tier flags are still always required separately (ADR-0009;
scripted teardown-and-reinstall is a first-class use case):

```bash
topctl uninstall --purge-data --confirm
```

A missing or unreadable store (`top.db`) means TOP can prove nothing and the
command exits 1 (`nothing installed`) — `topctl doctor` is the tool
for that state.

## What uninstall actually does

In order, one idempotent body:

1. **Lease** — acquires the controller's serialization lease so nothing new
   starts while teardown runs (released on any exit).
2. **Fresh snapshot + two-key match** — every ledger entry is checked against
   a fresh host inventory; a resource must match both the ledger and the host
   (label/name) to be deletable.
3. **Apps** — ledgered TOP apps are removed through the Dokku adapter in
   reverse-namespace order (the `core` controller last), their storage mounts
   detached first when `--purge-data` is set. Each ledger row is removed only
   after a successful removal, so interrupted runs re-do exactly the rest.
4. **Report** — prints `Removed` / `Skipped (with reason)` / `LeftAlone (kept)` —
   the ledger-diff proof that pre-existing resources survived.
   `--dry-run` prints the same decision list with zero mutations.

## The Dokku-uninstall manual runbook (implemented M2 behavior)

`--uninstall-dokku` does **not** auto-run Dokku's purge in the M2 slice —
that auto-run is M3 hardening behind a separate confirmation. Instead the
command prints exactly what remains and the exact commands to run by hand
(verified against the implementation's report content; these lines appear in
the uninstall report's `Notes`/`Commands` sections):

```text
uninstalling dokku — its own uninstaller leaves (check each): apt/dpkg package files (dokku, herokuish, plugn, sshcommand); /var/lib/dokku (plugin data, storage dirs, nginx state); /etc/nginx config rendered by dokku; the `dokku` unix user and its sshcommand.authorized_keys
sudo apt-get --purge -y remove dokku
sudo deluser --remove-home dokku
sudo rm -rf /var/lib/dokku
```

Similarly, a refusal case: networks and volumes without TOP's labels (e.g.
Dokku-created networks) are refused by the two-key rule and reported with the
manual command to review, e.g. `dokku network:destroy <name> for networks;
review by hand` — run those only when you have verified the resource is
yours.

## What remains afterwards (by design)

- Without `--purge-data`: all workload data volumes, the `top-metadata`
  storage entry, and the rest of the data dir.
- Without `--purge-secrets`: `<dataDir>/secrets/` and `<dataDir>/git/`
  (deploy key, known_hosts).
- Whatever Dokku's own uninstaller leaves (listed above; TOP reports it
  rather than cleaning blindly).
- The pre-install inventory and final diff report — proof that only TOP's
  resources were removed.
