# internal/gitsource — the Git layer of the controller (T-301)

The GitSource is the ONLY package that touches `git` argv. It answers two
questions for the controller (T-302/T-310): *what is the latest revision of
the platform repo?* and *what is in a given revision?* Contract:
[`docs/contracts/git-source.md`](../../docs/contracts/git-source.md),
decisions: [ADR-0008](../../docs/adr/0008-secrets-and-git-keys.md) (deploy
keys), [ADR-0003](../../docs/adr/0003-trigger-model-webhooks-poll.md) (poll
default, 60 s canon).

## API

- `Source` — from `config.Config` (`git.url`, `git.branch`, deploy-key path,
  known_hosts path under `dataDir/git/`). Key and known_hosts are only ever
  **referenced by path** (0600/0600 on disk) and passed through
  `GIT_SSH_COMMAND` (`-i <key> -o IdentitiesOnly=yes` +
  `StrictHostKeyChecking=yes` against the pinned known_hosts) — never on
  argv, never read into the process.
- `EnsureMirror(ctx)` — one-time `git clone --mirror` into the mirror dir
  under the TOP data area; subsequent fetches go to the mirror, not the
  network remote.
- `ResolveLatest(ctx)` (string sha, error) — `git ls-remote <url> <branch>`;
  the poller's target resolution.
- `FetchRevision(ctx, sha)` (*Snapshot, error) — fetch the sha onto the
  mirror, then `git archive`-extract it into a deterministic temp workspace;
  `Snapshot` hands the extracted path to the caller (spec parsing is
  `internal/spec`'s job, not ours).
- `NewPoller(interval)` — the T-302 poll loop consumes this; interval 0
  means ADR-0003's 60 s canon (`config controller.pollIntervalSeconds`
  defaults to the same value).

## Typed errors

`ErrNotFound` (`ErrRevisionGone` — the force-push/sha-gone case: loud
failure, no mutation), `ErrAuthFailed`, `ErrHostKey`, `ErrUnreachable`
(auth/unavailable family), plus per-call classification
(`KindHostKey`, …). Host-key mismatch is ALWAYS a loud typed failure —
there is no auto-accept and no `StrictHostKeyChecking=no` path.

## Testing tiers

- Tier 1: `gitsource_test.go` — golden per-command argv tables (`ls-remote`,
  `fetch`, `archive`), `GIT_SSH_COMMAND` construction + env-carrying
  evidence (key contents never appear), exit-code → typed-error mapping
  (128 → auth vs unavailable), tempdir lifecycle, archive extraction from a
  tiny in-process `archive/tar` stream.
- Tier 3 (`TOP_E2E=1`, `//go:build e2e`, `e2etest/`): a disposable local git
  server container serves a fictional bare repo; proofs: private-repo clone
  via a generated deploy key (ssh-keygen in the test), host-key mismatch
  fails loudly, sha-gone error path. Docker Desktop-runnable on macOS; never
  pointed at a shared host.

## Handoff notes (T-302 / T-310)

`FetchRevision` is the consumer entrypoint of the reconciler's spec load;
`Event`-like trigger logic is NOT here — webhook payload routing lives in
`internal/webhook` (width-first dedup there), and the poll loop coalescing
dedup lives with the controller (T-302). Poll defaults may be overridden by
`config controller.pollIntervalSeconds`; the poller honors both.
