# Installing TOP

> Status: **real (M2)** — matches the implemented `topctl install`
> (T-205..T-207), the CLI surface of `docs/contracts/cli.md`, and
> `install-top.sh` (T-209). Owner rules: ADR-0005 (bootstrap), ADR-0009
> (ownership/approval). Commands below were verified against `topctl dev`
> (build of this repo) unless marked otherwise.

## Prerequisites

- A Linux host you can dedicate to TOP (recommended) or share under the
  ownership rules in ADR-0009. Supported: Ubuntu 22.04/24.04/26.04 or
  Debian 11+ (x86_64 or arm64). macOS runs only in a development-only tier
  (doctor warns, never installs).
- Minimum resources: 1 vCPU / 1 GB RAM for TOP + Dokku alone;
  **2 vCPU / 4 GB RAM / 20 GB free disk** recommended with the observability
  stack. Small-host tuning: see `observability.md`.
- SSH access with `sudo` (interactive is fine; TOP prints exact sudo commands
  and never assumes passwordless sudo).
- Docker installed and the daemon reachable, or allow the gated Dokku install
  to bring it. Docker engine ≥ 24.0; Dokku ≥ 0.38 when already present.
- A Git repository of ServiceSpecs readable via a read-only deploy key.
  The committed example repo (`examples/platform-repo`, T-401's template
  fleet) ships with TOP, and `--init` can seed one initial commit into a
  remote the operator created empty (T-212; see `installer.md` §`--init`).
- Ports 80/443 free if you will use HTTP proxying (skippable with
  `--skip-ports`).

## The one-liner

```console
$ ./install-top.sh https://github.com/example/platform-repo.git
install-top: preflight (read-only) —
doctor: doctor found no failures, 3 warning(s)
...
```

The script is thin and inspectable (POSIX sh, ~70 lines). It only ensures a
`topctl` binary and forwards **everything** to `topctl install` — which itself
preflights, plans, and approval-gates. The script never installs Go, never
edits `PATH`, and makes no host changes beyond the ones `topctl install`
performs at your request.

Getting `topctl`, in order of preference:

1. **On PATH** — use it as-is.
2. **Pinned release artifact** — downloaded from a version-pinned base URL
   (`TOP_ARTIFACT_BASE_URL` at the top of the script); the script verifies the
   accompanying `.sha256` file **when the release publishes one** (checksums
   live in the release notes; the script grants no cryptographic trust to the
   environment beyond that).
3. **Build from source** — `go build -o ./topctl ./cmd/topctl` (needs
   Go ≥ 1.24).

## Preflight first

`install-top.sh` runs `topctl doctor` (read-only, phase 0) before every
install and prints its findings, then continues; the installer itself
re-preflights and stops on real failures. Run it alone with:

```console
$ ./install-top.sh --check   # doctor only, change nothing
```

On a macOS development machine (`topctl dev`), the expected result is a
**warning** report that still exits 0:

```text
doctor: doctor found no failures, 3 warning(s)
  warn [os/os]                operating system: macOS (darwin/arm64) is not a supported host; TOP targets Linux — this counts as a development-only run
        hint: use a Linux host (Ubuntu 22.04/24.04/26.04, Debian 11+) for a real install; on macOS only dev-tier tooling is expected to run
  ok   [docker/version]       docker engine version: docker engine 29.9.2 satisfies the 24.0 floor
  info [dokku/state]          dokku present: dokku is not installed; TOP can install it (approval-gated)
  ok   [ports/80]             port 80 free: port 80 is bindable (not currently held)
  ok   [ports/443]            port 443 free: port 443 is bindable (not currently held)
  warn [resources/disk]       disk headroom: 16 GiB free on /var/lib; below the 20 GiB recommended headroom
  ok   [resources/ram]        memory headroom: 8 GiB RAM (≥ 4 GiB recommended with the observability stack)
  warn [sudo/passwordless]    passwordless sudo: `sudo -n true` did not succeed; non-interactive install steps will need you at the keyboard once
  info [store/datadir]        data directory /var/lib/top: /var/lib/top does not exist yet; created by `topctl install` (0700)
```

(Illustrative Mac run shown verbatim from testing; your versions/disk will
differ.) On a **clean Linux VM** — the T-209 acceptance case — the same check
is expected to be *fully green* (no failures, only the informational dokku and
data-dir notes), exiting 0. That VM result still needs to be confirmed on the
e2e VM.

## The explicit form

Prefer the explicit form when calling `topctl` directly from scripts or
agents — these flags come straight from `topctl install --help` (verified):

| Flag | Meaning |
| --- | --- |
| `<repo>` | Git URL of the platform repo (exact-args: `https://…`, `ssh://…`, or `git@host:path`) |
| `-o, --output text\|json` | output format (default text; `json` = versioned envelope, `agent-consumable`) |
| `--confirm` | non-interactive approval for the gated command (ADR-0009) — interactive TTY is the default |
| `--dry-run` | print the plan, mutate nothing |
| `--install-dokku` | also install Dokku if absent (approval-gated change list: apt packages, nginx on 80/443, `dokku` user, sshcommand) |
| `--deploy-key <path>` | operator-supplied private deploy key (provisioned 0600 into `<dataDir>/git`; **required in v0**) |
| `--known-hosts <path>` | pinned `known_hosts` file (provisioned 0600 into `<dataDir>/git`; **required in v0**) |
| `--controller-image <ref>` | override the pinned bootstrap controller image |
| `--controller-tag <tag>` | override the pinned bootstrap controller tag |
| `--skip-ports` | skip the read-only 80/443 availability probe |

Gate facts, verified: `--confirm` is required (prompt or flag) or the command
exits 2 with usage guidance; dry-run needs no approval because nothing
mutates; there is **no yes-to-everything switch**.

```console
$ ./topctl install https://github.com/example/platform-repo.git --dry-run \
    --deploy-key /home/you/.ssh/top_deploy_key --known-hosts /home/you/.ssh/top_known_hosts
$ ./topctl install https://github.com/example/platform-repo.git \
    --deploy-key /home/you/.ssh/top_deploy_key --known-hosts /home/you/.ssh/top_known_hosts
```

## What happens (implemented phases)

Phases 0–6 run and are reported by `topctl install` (the labels mirror
`Result.PhasesRun` — `internal/install/phases.go` `phaseNames`, verified
per the dry-run integration tests), matching
`docs/contracts/installer.md`'s 0–6 numbering:

| # | Phase label | What it does |
| --- | --- | --- |
| 0 | `0-preflight` | `topctl doctor` + installer-specific checks (repo reachable/non-empty per ADR-0005, deploy-key material paths). Finder-once: all failures display together. |
| 1 | `1-inventory` | Snapshot existing containers/images/networks/volumes/listeners/Dokku apps into the resource ledger as `pre-existing` (ADR-0009 — the uninstall ledger's anchor). |
| 2 | `2-dokku-setup` | With `--install-dokku`: run the pinned upstream bootstrap after explicit confirmation (or the exact sudo commands as a fallback; ADR-0009). Without it: stop with instructions when Dokku is missing; verify version/health when present (≥ 0.38). |
| 3 | `3-platform-wiring` | `<dataDir>/{secrets,git,backups}` at 0700, `top-internal` network, provision the deploy key + pinned known_hosts, register the repo and verify resource resolution. |
| 4 | `4-bootstrap-deploy` | One-time direct Dokku deploy of `core-top-controller` from the pinned bootstrap image, with the `top-metadata` SQLite storage mount; recorded in the ledger as `bootstrap-owned`. |
| 5 | `5-first-reconcile` | One reconciler pass (the install CLI's `FirstRunner` seam, `internal/install/adopt.go`): validates/loads the spec — including the example/fleet repo — and hands ownership of the deployment over per ADR-0005: the ledger row converts `bootstrap-owned → managed` (`store.MarkManaged`), annotated in the phase-5 op record (`adoptedByOp`/`firstRunOp`; owner-blessed 2026-11-04, R3 — installer.md phase 5 + ADR-0005 amendment). |
| 6 | `6-report` | Prints what was created / adopted / left alone, which secrets still need provisioning, and suggested next commands. |

> **Contract alignment note:** phase 5 (first reconciliation) and phase 6
> landed in M4 (T-402). The wording drift against `installer.md` ("the
> bootstrapped controller loads the repo" vs the composition-root phase-5
> pass from the install CLI) was m4-review.md §4 **R3** — owner-blessed
> 2026-11-04; `installer.md` phase 5 now carries the composition-root
> clause and ADR-0005 carries the op-record provenance amendment.

Resumability: every phase records a store operation (`kind: install`,
`details.phase`), so re-running `topctl install` (same command, same flags)
resumes or skips at the completed phases. On a phase failure, install prints
the failing command, its output, and the resume command; prior phases stay
touched. A bootstrap-deploy failure leaves no partially-adopted state — the
ledger records `bootstrap-failed` and `topctl recover diagnose` explains the
next steps.

## After install

- Provision secrets with the v0 host-file provider (0600 under
  `<dataDir>/secrets`, referenced via `secretRefs`; ADR-0008/0013).
  **`topctl secrets set` is not yet exposed on the CLI** (no `secrets`
  subcommand exists in `topctl` yet — the wiring lands with M2/M3 secret
  work). Until then, files provisioned into `<dataDir>/secrets/` (0600) are
  the working surface.
- Webhooks (optional): point the Git provider at the controller's
  `/webhooks/github` endpoint with the shared secret — or do nothing and rely
  on the documented default of **poll-only sync** (60 s poll + hourly
  reconcile; ADR-0003). Webhooks are an optimization, never a requirement.
- Watch it converge: once the controller is reconciling, `topctl diff`
  (exit 0 = converged, 1 = drift present) and `topctl history` show the
  per-service state; `topctl status` is still an honest stub (`topctl
  diff` is the real drift surface today).

Exit codes (CLI contract): 0 ok; 1 preflight/diagnose findings; 2 usage; 3
gate/sudo refusal or phase failure; 4 store/backend unavailability.

## What can go wrong

| Symptom (what you saw) | Cause | Fix |
| --- | --- | --- |
| `install: repo "…" does not look like a git remote (want https://…, ssh://…, or git@host:path)` (exit 2) | malformed repo argument | use a full `https://github.com/example/platform-repo.git`-style URL; `topctl install --help` for the surface |
| `install: preflight failed: doctor found N failure(s), M warning(s)` (exit 1) | phase-0 failures (missing docker, wrong OS tier, port 80/443 held, too little disk/RAM, name collision with a reserved namespace) | run `./install-top.sh --check` and fix each named failure with its printed hint |
| `install: preflight failed` mentioning `repo/reachable` | repo missing/empty/unreachable without `--init` | push the repository first, or pass `--init` against an EMPTY remote (T-212: `installer.md` §`--init`); ADR-0005: no silent repo mutation |
| `install: preflight failed` mentioning `install-input` | `--deploy-key`/`--known-hosts` paths not given or unreadable | supply both files (required in v0); make sure the key exists and the user can read it |
| install exits **2** with "gated" guidance | ran a mutating command without `--confirm` | add `--confirm` (deterministic path) or answer the interactive prompt |
| sudo dialog appears mid-install | ADR-0009 prints exact host commands and never assumes passwordless sudo | confirm once at the keyboard; for scripting, configure passwordless sudo for the install user |
| `install: … store unavailable` (exit 4) | SQLite store cannot open `<dataDir>/top.db` (permissions, disk) | check the data-dir permissions / disk space, then re-run |
| bootstrap deploy fails (exit 3, ledgered `bootstrap-failed`) | image pull, mount, or Dokku wiring | `topctl recover diagnose` prints the manual path (`dokku git:from-image …`) and the resume command |

## Uninstall

See [teardown.md](teardown.md).
