# Contract: GitSource / GitController

Owns the relationship with the platform Git repo. Access decisions in
ADR-0008 (deploy key + pinned known_hosts, git CLI over SSH/HTTPS); trigger
semantics in ADR-0003.

## Responsibilities

- **Registration**: bind TOP to a repo URL + branch at install; validate
  access and host key before marking install complete. Two cases
  (ADR-0005): (a) repo exists with content → register and sync, change
  nothing; (b) repo missing/empty and `--init` given → render the embedded
  platform-repo template, create the remote repo where the provider
  supports it, push the initial commit using a one-time write-capable
  credential (never stored), then proceed as (a). Without `--init`, a
  missing/empty repo is a preflight error.
- **Clone/fetch**: maintain a bare mirror under `/var/lib/top/git/mirror.git`
  via the exec layer with `GIT_SSH_COMMAND` (strict host-key checking against
  the pinned `known_hosts`). Shallow where possible.
- **Revision resolution**: `Resolve(branch) → sha` (`git ls-remote`), and
  `Fetch(sha)` so any referenced sha is locally present.
- **Materialization**: `CheckoutFiles(sha, dest)` — extract
  `<sha>:<namespace>/*.yaml` to a temp dir for spec loading
  (`git archive <sha> | tar -x`), never a working-tree checkout shared across
  runs.
- **Polling**: 60 s loop calling `Resolve`; change → enqueue reconcile.
- **Webhook intake**: see ADR-0003 (HMAC verify, delivery dedup, 202+enqueue;
  payload content never used as desired state).

## Error semantics

- Auth/host-key failure → `ErrGitAuth` with a remediation hint (deploy key
  not granted / known_hosts mismatch) — **never** auto-bypass verification.
- Network/remote failure → `ErrGitUnavailable`, retried with backoff
  (ADR-0003); a failed fetch aborts the run *before* mutation.
- sha vanished (force-push) → `ErrRevisionGone`; the run fails and alerts
  (someone rewrote the source of truth — human should look).

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
