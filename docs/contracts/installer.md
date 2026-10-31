# Contract: Installer / Bootstrap

`./install-top.sh <git-repo-url>` → `topctl install <git-repo-url>`. Bootstrap
topology and hand-off rules in ADR-0005; ownership/approval rules in ADR-0009.
Everything below is `--dry-run`-able and idempotent (re-running resumes).

## Phases

**0. Preflight (`topctl doctor`)** — read-only, always safe:
OS/arch in supported set; Docker present + daemon reachable + version floor;
Dokku: absent / present-and-version-≥0.38 / present-broken; ports 80/443 free
*if* proxying will be used; RAM/disk vs documented minimums; name collisions
with reserved namespaces; sudo availability (interactive is fine — TOP prints
exact sudo commands, never assumes passwordless).

**1. Ownership inventory** — snapshot existing containers, images, networks,
volumes, listeners, and (if present) Dokku apps into the ledger as
`pre-existing`. This snapshot is what uninstall later uses to prove it
touches nothing foreign (ADR-0009).

**2. Dokku setup** — if absent: `--install-dokku` runs the documented upstream
bootstrap **after explicit interactive confirmation** listing changes (apt
packages, nginx on 80/443, `dokku` user, sshcommand). Without the flag: stop
with instructions. If present: verify version/health.

**3. Platform wiring** — create `/var/lib/top/{secrets,git,backups}` (0700),
provision deploy key + pinned known_hosts (operator supplies), create the
shared internal network (`network:create top-internal`), register the repo
and verify `Resolve(branch)` works. If the repo is missing/empty and
`--init` was passed — see the `--init` notes below; otherwise missing/empty
repo = stop with instructions.

**4. Bootstrap deploy** — one-time direct Dokku deploy of
`core-top-controller` from the pinned bootstrap image, with its SQLite
storage mount. Recorded in the ledger as `bootstrap-owned`.

**5. First reconciliation** — the bootstrapped controller loads the repo,
adopts its own deployment (same app name; converts ledger entry to
`managed`), and reconciles `core` then `observability` per ADR-0002. Install
exits 0 only when `Readiness(core)` is green and the first run is recorded.

**6. Report** — prints: what was created, what was adopted, what was left
alone, where secrets must still be provisioned, and the next suggested
commands.

## Teardown (`topctl uninstall`)

Order: stop accepting triggers → `controller lease` acquired → remove
ledgered managed apps via Dokku (reverse namespace order) → detach TOP
storage mounts → with `--purge-data`: destroy storage entries + `/var/lib/top`
+ backup dir (kept by default!) → with `--purge-secrets`: remove secrets +
git keys → with `--uninstall-dokku`: run Dokku's documented purge after a
separate confirmation that prints its leftovers → final ledger diff report
proving pre-existing resources survived.

## `--init` (as implemented in T-212, subtitled truth per internal/install)

`topctl install <repo> --init` seeds ONE initial commit into a platform
remote the operator created EMPTY (the remote itself is **never created**
via a provider API — a deliberate v0 deferral of the ADR-0005 "provider
permitting" clause). It pushes TOP's embedded platform-repo template (same
content as `examples/platform-repo`, single source of truth). Typed
refusals: unreachable remote under `--init` → `ErrRemoteUnreachable`; a
non-empty remote refuses with the remote's evidence — never forced, merged,
or overwritten; a re-run recognizes its own seed and resumes. Credentials
(ADR-0008): https remotes take a one-time write token from the
`TOP_INIT_TOKEN` environment ONLY, carried through a 0700 askpass shim
deleted after the push (never argv, never git config, never on disk, never
logged); ssh remotes authenticate with the operator's deploy-key FILE PATH
inside `GIT_SSH_COMMAND` — if that key is read-only the push fails loudly
and the operator switches to https + `TOP_INIT_TOKEN`. Steady-state
transport stays the read-only deploy key.

## Failure behavior

- Any phase failure: stop, print the failing command, its output, the resume
  command (`topctl install` re-run), and leave prior phases untouched.
- Bootstrap deploy failure leaves no partially-adopted state: the ledger
  marks `bootstrap-failed` and `topctl recover diagnose` explains next steps.
