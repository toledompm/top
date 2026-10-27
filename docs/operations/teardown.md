# Tearing down TOP

> Status: **draft** — target behavior (Milestone 2). Ownership and approval
> rules: ADR-0009.

## Principles

- TOP deletes **only** what it can positively identify as its own: the
  resource must be in TOP's ledger *and* carry TOP's labels/name prefix.
  Disagreement → skipped with a warning, never guessed.
- Foreign containers, volumes, networks, firewall rules, and proxy config are
  never touched.
- Data is kept by default; every destructive tier is a separate flag with a
  separate confirmation.

## Command

```bash
topctl uninstall --dry-run          # the full plan, nothing touched
topctl uninstall                    # removes TOP-managed apps + mounts
topctl uninstall --purge-data       # + storage entries, /var/lib/top, backups
topctl uninstall --purge-secrets    # + secrets and git credentials
topctl uninstall --uninstall-dokku  # + Dokku itself (separate confirmation;
                                 #  prints what Dokku's purge leaves behind)
```

Non-interactive use (agents, CI, e2e): add `--confirm` to skip the
interactive prompts — the destructive flags are still always required
(ADR-0009). Full scripted cycle:
`topctl uninstall --purge-data --confirm && topctl install <repo> --install-dokku --confirm`.

## What remains afterwards (by design)

- Without `--purge-data`: all workload data volumes and TOP backups.
- Without `--purge-secrets`: `/var/lib/top/secrets/` and `/var/lib/top/git/`.
- Whatever Dokku's own uninstaller leaves (documented upstream; TOP reports
  it rather than cleaning blindly).
- The pre-install inventory and final diff report, printed and written to
  the last ledger export — proof that only TOP's resources were removed.
