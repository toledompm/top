# internal/doctor

Read-only preflight for `topctl doctor` — installer Phase 0
(docs/contracts/installer.md, ADR-0009, ADR-0001). **Doctor never mutates
the host**: it reads filesystem/process facts and runs a handful of
version/health probes through `internal/exec`. Fix suggestions are
printed; anything that changes the host (installing Docker/Dokku,
tightening `dataDir` to 0700, firewall changes) belongs to
`topctl install`'s gated phases — doctor only points at them.

## Import note

This package deliberately does **not** import `internal/cli`
(`internal/cli` imports doctor to make the command real), so the render
helpers in `render.go` mirror the cli envelope style in spirit only. The
tiny shared shape is documented there and in `scripts`-free form:
`{"schemaVersion":1,"command":"doctor","ok":…,"findings":[…]}`. Two
constants are duplicated on purpose (`MinDokkuVersion = 0.38` mirrors
`cli.MinDokkuVersion`, per ADR-0001) — keep both in sync.

## API

```go
type Finding struct{ ID, Severity, Check, Message, Hint string }
// Severity ∈ {ok, info, warn, fail}
type Report struct{ Findings []Finding }
func (r Report) OK() bool          // no fail findings?
func Run(ctx, Options) Report
func (r Report) RenderText(w) error
func (r Report) RenderJSON(w) error   // standalone shape (see above)
func (r Report) EnvelopeData() map[string]any  // payload for cli's envelope
```

`Run` never errors: every per-check problem is a `Finding`. `Report.OK()`
is the exit-code gate.

## Checks and their semantics (choices recorded)

| ID | Severity fan-out | Semantics |
| --- | --- | --- |
| `os/os` | linux ok / **warn on darwin** (documented dev-run, ADR-0012 dev story) / fail otherwise | Supported hosts: linux amd64/arm64. macOS = dev only, warn not fail. |
| `os/distro` | warn/fail per window | `/etc/os-release`; Ubuntu 22.04/24.04/26.04 and Debian 11+ supported (docs/operations/installation.md). Unreadable → warn. Other distro → **warn** (untested but plausible), unsupported version → fail. |
| `docker/binary` | fail if not on PATH | Hint points to the gated install bootstrap (`--install-dokku`), never to a silent install. |
| `docker/daemon` | fail if `docker version` exits non-zero | Reachability, not identity. |
| `docker/version` | fail if engine < floor | Floor: **`24.0` — recorded assumption**, no platform doc pins a Docker floor yet; 24.0 was the stable engine line at planning time, and Dokku's own install manages Docker. Data: stderr excerpt, exit code. Bump the floor + this note together. |
| `dokku/state` | **absent → info** (guided install exists — NOT a failure) / ok at ≥ `0.38` (ADR-0001) / fail if present but `dokku version` fails, below 0.38, or is unrunnable by this user | Broken/permission cases: exact fix hints (group membership, sudo -E, reinstall). Absent → info pointing at `topctl install --install-dokku` (approval-gated). |
| `dokku/apps` + `names/*` | fail for an existing `core-top-controller`; warn for other `<reserved-ns>-`prefixed apps | One read-only `dokku apps:list` when dokku is healthy. Controller-name collision is a hard fail (Phase 4's bootstrap deploy needs that exact name, ADR-0009). |
| `ports/80`, `ports/443` | ok/warn | Always probed by default per the brief's simplification (owned by proxy-mode or not — no firewall probing). Occupied → warn with the "Dokku's nginx is the expected owner" note; **EACCES/EPERM** (privileged port, unprivileged caller) → warn "cannot verify read-only". Opt-out: `topctl doctor --skip-ports`. Probe is bind-then-immediately-close via net.Listen — the **TOCTOU window** between our close and the installer's bind is a documented race, not a guarantee. Never fails (occupied is a proxy-mode problem, not a platform-will-crash problem). |
| `resources/disk` | ok ≥ 20 GiB / warn < 20 GiB / **fail < 2 GiB** | Anchored at `dataDir` (or its **nearest existing ancestor** — no writes, no mkdir). 20 GiB is the post-laydown recommendation (installation.md); 2 GiB is the recorded local lay-down floor (TOP's own choice, doc'd here as the fail line). |
| `resources/ram` | ok ≥ 4 GiB / warn 1–4 GiB / **fail < 1 GiB** | 1 GiB = TOP+Dokku-alone floor; 4 GiB = the recommended observability stack point (installation.md). Linux: `/proc/meminfo` MemTotal. macOS: `sysctl -n hw.memsize` via the Runner — dev-run accuracy. |
| `sudo/passwordless` | ok / warn (never fail) | `sudo -n true`. Missing passwordless sudo = fine (ADR-0009: TOP prints exact sudo commands; interactive is sanctioned). Non-root, non-scripting users just accept the warn. |
| `store/datadir` | info/warn/ok only | **No write test** — the doctor contract says read-only; the mkdir+chmod 0700 is the installer's move. Doctor reports existence + observed perms (≥ AS-given: warns when mode is looser than 0700 or the path isn't a dir). |

Exit-code contract (documented + tested — see `internal/cli/doctor_test.go`):
**0** when `Report.OK()` is true (fails-free, including warns);
**1** when at least one fail exists (the "diagnose findings" row of
docs/contracts/cli.md). Warns/info never change the exit code — they are
advisories, not blockers.

## Seams (all tests use them; production defaults are the

workflow-normal bits: real `exec.FastExec`, real listeners, real syscalls)

```go
Options{
  Runner exec.Runner        // FakeRunner in tests; FastExec in production
  OSProbe OSProbe           // fakeable uname
  Probe  ListenProbe        // fakeable bind/close (tests never bind :80+)
  MemProbe / DiskProbe      // fakeable /proc+statfs readers
  DataDir, ReservedNamespaces, SkipPorts
}
```

`run` ordering is fixed by `Run`'s body; all checks run even when some
fail (fixes-at-a-glance).

## Test grid (doctor_test.go)

- Tables over FakeRunner per probe family (docker, dokku, sudo, sysctl).
- Port probe / mem / disk / OS via injected funcs; **no :80/:443 or
  /proc reads in tests**.
- exit-code contract: fail → 1; warn-only and clean → 0.
- JSON and text goldens render side by side.
