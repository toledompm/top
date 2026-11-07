# Backups

Scope: ADR-0013 (boundary) and ADR-0007 (TOP's SQLite state). Backups are
the operator's job: TOP ships no backup machinery — no scheduler, no CLI
verbs, no off-host copy wiring.

## The one-line boundary

**Your apps' data is yours.** TOP itself needs no restore ritual: its own
metadata (the SQLite `top.db`) is reconstructible by re-running operations
(`topctl install` + reconcile, which is idempotent). The recipes below are
the operator's insurance against losing the battle history recorded there —
operation/deployment records, the resource ledger, checkpoints — which is
convenient to have but not load-bearing.

## What exists where

| Location | Contents |
| --- | --- |
| `/var/lib/top/top.db` (Dokku storage mount `top-metadata`) | TOP's SQLite metadata (no secret values — ADR-0008) |
| `/var/lib/top/secrets/` | Secret values (re-provision out-of-band; names + checksums only) |
| `/var/lib/top/secrets/git/` | Git read-only deploy key (re-provision per the runbook) |
| Application volumes / databases | The operator's app-level backups |

## The operator recipe (TOP's metadata)

Everything you need is SQLite tooling on the host.

1. **Snapshot** (consistent, WAL-safe — never a raw `cp` of the live file):

   ```console
   $ sqlite3 /var/lib/top/top.db "VACUUM INTO '/backups/top/top-<timestamp>.db'"
   ```

   `VACUUM INTO` produces a fully-compacted, consistent copy taken
   against a read transaction; it is safe while the controller is up
   (WAL guarantees the snapshot). Rotate the timestamp yourself; keep the
   destination append-only by convention — never overwrite a previous
   backup file.

2. **Prune** (a plain retention policy suggestion: keep the newest backup
   per distinct day for **14 days**, then the newest per distinct ISO week
   from the remainder for **8 weeks**; the newest backup always survives).
   A dozen lines of `find`/`stat` in the operator's toolbag — TOP imposes
   no pruning.

3. **Off-host copy (your transport)**: rsync/SSH to a host of your choice
   is the strongest habit for one host; a copy elsewhere is what turns a
   disk failure into an inconvenience.

## Verify a snapshot

Open the copy strictly read-only and check integrity:

```console
$ sqlite3 'file:/backups/top/top-<timestamp>.db?mode=ro' \
    'PRAGMA integrity_check;'
```

`PRAGMA integrity_check` must answer `ok`.

## Restoring

Drop the snapshot in place (with the controller stopped —
`dokku ps:stop core-top-controller`), start the controller afterward
(`dokku ps:restart`), and let reconcile converge from whatever the
operations re-run to. If the copy is older than the live DB's newest
recorded operation, the re-run rebuilds the missing slice — reconcile is
idempotent, so the result converges on current Git state either way.

## Application-data cookbook (operator's, unautomated)

- Per-app database dumps as ordinary TOP-managed cron workloads writing to a
  mounted volume you snapshot at the host level.
- Host-level filesystem/LVM/ZFS snapshots of `/var/lib/dokku/data/storage/`.
- Off-host sync of storage dirs while the owning app is stopped (quiesce
  first; Dokku storage survives app rebuilds but live DB files don't
  like naive copies either).
