# Installing TOP

> Status: **draft** — describes the target behavior (Milestones 2–4). Commands
> become real as those milestones land.

## Prerequisites

- A Linux host you can dedicate to TOP (recommended) or share under the
  ownership rules in ADR-0009. Supported: Ubuntu 22.04/24.04/26.04 or
  Debian 11+ (x86_64 or arm64).
- Minimum resources: 1 vCPU / 1 GB RAM for TOP + Dokku alone;
  **2 vCPU / 4 GB RAM / 20 GB free disk** recommended with the observability
  stack. Small-host tuning: see `observability.md`.
- SSH access with `sudo` (interactive is fine; TOP prints exact sudo commands
  and never assumes passwordless sudo).
- Docker installed, or allow the gated Dokku install to bring it.
- A Git repository of ServiceSpecs (see `examples/platform-repo/`), readable
  via a read-only deploy key you can provision on the host.
- Ports 80/443 free if you will use HTTP proxying.

## Install

```bash
./install-top.sh github.com/example/my-platform
```

Equivalent explicit form:

```bash
topctl doctor                      # read-only preflight; fix anything it flags
topctl install github.com/example/my-platform --dry-run   # full plan, no changes
topctl install github.com/example/my-platform             # prompts at each gate
```

What happens (details: `docs/contracts/installer.md`): preflight → ownership
inventory → optional gated Dokku install (`--install-dokku`) → secrets/git
wiring → one-time bootstrap deploy of the controller → first reconciliation
adopts everything, including the controller itself.

**No platform repo yet?** Pass `--init` and TOP creates the repository from
its embedded template (`core/top-controller.yaml`, `observability/*`,
README — the same content as `examples/platform-repo/`) and pushes the
initial commit using a one-time write-capable credential you supply at the
prompt; steady-state syncing then runs on the read-only deploy key. If the
repo already exists with content, TOP registers it and starts syncing
without changing anything.

## After install

- Provision secrets: `topctl secrets set grafana-admin-password` (reads stdin).
- Check: `topctl status`; drift preview: `topctl diff`.
- Webhooks (optional): point your Git provider at the controller's
  `/webhooks/github` endpoint with the shared secret — or do nothing and rely
  on the 60 s poll + hourly reconcile. Webhooks are an optimization, never a
  requirement.

## Uninstall

See [teardown.md](teardown.md).
