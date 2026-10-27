# Example `.local/` setup (fictional values — safe to commit)

Contributors may keep a gitignored `.local/` directory with machine-specific
notes. This example shows the shape with **fictional** values — copy it to
`.local/` and adjust. Never commit the real thing.

## `.local/testhost.md` (example)

```markdown
# My test host (private)

- SSH: `deploy@apps.example.com` (key at ~/.ssh/example_ed25519)
- OS: Ubuntu 24.04, 2 vCPU / 4 GB — disposable VM, rebuild via `tart recreate testvm`
- Dokku: installed per docs/operations/installation.md on 2026-10-27
- Local overrides: TOP_E2E_HOST=apps.example.com TOP_E2E_SSH_USER=deploy

## Notes

- Rebuilt monthly; nothing on it matters.
- Webhook reachability: VM has no public address, so I test webhooks with
  `topctl reconcile --now` plus the manual redelivery endpoint instead.
```

## Guidelines

- Prefix env overrides in `.local/env` and source them only in your shell.
- If a local detail changes how the product should behave, generalize it into
  an ADR or architecture note instead of documenting the host itself.
