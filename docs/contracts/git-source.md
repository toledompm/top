# Contract: GitSource / GitController

Owns the relationship with the platform Git repo. Access decisions in
ADR-0008 (deploy key + pinned known_hosts, git CLI over SSH/HTTPS); trigger
semantics in ADR-0003.

## Responsibilities

- **Registration**: bind TOP to a repo URL + branch at install; validate
  access and host key before marking install complete. Two cases
  (ADR-0005): (a) repo exists with content → register and sync, change
  nothing; (b) repo missing/empty and `--init` given → render the embedded
  platform-repo template (single source of truth shared with
  `examples/platform-repo`) and push ONE initial commit using a one-time
  write-capable credential, never stored (see installer.md — the
  remote itself is NOT created via a provider API; v0 deferral). Without
  `--init`, a missing/empty repo is a preflight error.
- **Clone/fetch**: `EnsureMirror(ctx)` — one-time `git clone --mirror`
  into the mirror dir under the TOP data area (`dataDir/git/`); subsequent
  fetches go to the mirror, not the network remote. Exec-layer driven with
  `GIT_SSH_COMMAND` (strict host-key checking against the pinned
  `known_hosts`).
- **Revision resolution**: `ResolveLatest(ctx) → sha`
  (`git ls-remote <url> <branch>`); the poller's target resolution.
- **Materialization**: `FetchRevision(ctx, sha) → Snapshot` — fetch the
  sha onto the mirror, then `git archive`-extract it into a deterministic
  temp workspace; `Snapshot` hands the extracted path to the caller (spec
  parsing is `internal/spec`'s job). Never a working-tree checkout shared
  across runs.
- **Polling**: 60 s loop calling `ResolveLatest`; change → enqueue reconcile
  (controller daemon, ADR-0003).
- **Webhook intake**: see ADR-0003 (HMAC verify, delivery dedup, 202+enqueue;
  payload content never used as desired state).

## Error semantics

- Auth/host-key failure → typed auth errors (`ErrAuthFailed`,
  `ErrHostKey` — `KindHostKey`… classification) with a remediation hint
  (deploy key not granted / known_hosts mismatch) — **never** auto-bypass
  verification.
- Network/remote failure → `ErrUnreachable`, retried with backoff
  (ADR-0003); a failed fetch aborts the run *before* mutation.
- sha vanished (force-push) → `ErrNotFound` wrapping `ErrRevisionGone`;
  the run fails and alerts (someone rewrote the source of truth — human
  should look).

## Security rules

- The deploy key is **read-only** on the provider side.
- `StrictHostKeyChecking=yes` always; no config path disables it.
- Nothing from a webhook payload reaches exec argv (repo/ref/sha are
  re-derived from the configured remote; payload used for audit only).

## Illustrative

```
register("git@github.com:example/my-platform.git", branch="main")
  → ls-remote resolves a1b2c3 → fetch → archive a1b2c3 → tmp dir
  → spec.Load(tmpdir) → 7 services validated
poll: ls-remote main → d4e5f6 ≠ a1b2c3 → enqueue reconcile
```
