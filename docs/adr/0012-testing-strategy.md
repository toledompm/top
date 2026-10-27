# 0012 — Testing strategy and the end-to-end environment

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: **yes** (e2e environment choice)

## Context

Acceptance criteria must be reproducible by contributors without private
infrastructure, while the owner also wants optional tests against a real
host. Dokku manages Docker, so naive "Dokku in Docker" is fraught; the test
pyramid must reflect that.

## Decision

**Four tiers:**

1. **Unit (hermetic, every PR)** — domain, spec parse/validate, plan/diff,
   reconcile decision logic, sqlite store (temp-file DBs), webhook HMAC/dedup,
   redaction. No host, no Docker, no network.
2. **Shim integration (every PR)** — the `exec.Runner` seam (ADR-0006) is
   exercised against a fake `dokku` shell script on PATH that records argv and
   returns canned stdout/exit codes. Verifies command construction, timeout,
   and redaction paths without Dokku.
3. **Real-Dokku integration (PR CI on Linux, opt-in locally)** — the official
   `dokku/dokku` Docker image with the host's docker.sock mounted (Dokku
   managing *sibling* containers — the documented topology), orchestrated by
   testcontainers-go. Covers the Dokku adapter against reality. Gated:
   `TOP_E2E=1` + Linux (works on macOS via Docker Desktop/OrbStack, documented
   but not CI).
4. **Full e2e (nightly/release; contributor-runnable)** — a disposable VM
   (Lima template committed to `test/e2e/`, cloud-init installing Docker +
   Dokku) running the scripted acceptance suite: install → reconcile core +
   observability → webhook change → missed-webhook poll recovery → manual
   diff/reconcile → invalid-spec rejection → controller restart mid-operation
   → TOP-down workload survival → independent recovery → backup/restore →
   ownership-safe uninstall. `test/e2e/run.sh` is one command end-to-end.

**Failure injection** lives in tiers 3–4: killed controller mid-deploy,
corrupted/wedged SQLite, failing healthchecks, invalid ServiceSpecs, Dokku
command failures, webhook floods/duplicates/out-of-order deliveries.

**Local homelab tests (optional, author-only)** — tier 4 scripts accept
`TOP_E2E_HOST`/`TOP_E2E_SSH_USER` overrides sourced from gitignored `.local/`;
read-only checks may run against a real host, but mutating/destructive
scenarios default to the disposable VM and require explicit `--allow-host-mutation`
plus interactive confirmation to point at anything else. No tracked test may
*require* a private host.

**CI**: GitHub Actions — tiers 1–2 on every PR (fast), tier 3 on PRs touching
`internal/backend/dokku` or nightly, tier 4 nightly + release tags. CI must
pass on a fork with zero secrets configured (tiers 1–3 only).

## Alternatives considered

- **Dokku-in-Docker-in-Docker (true DinD)** — flaky; the sock-mount sibling
  topology is the documented pattern. Rejected.
- **Only VM-based e2e** — too slow for PR feedback; tier 3 exists to catch
  adapter drift early. Rejected as sole e2e.
- **Mock-Dokku HTTP API** — Dokku has no API; it's a CLI. Not applicable.
- **Author's homelab as the CI e2e host** — violates ADR-0014 and makes a
  private host load-bearing for validating product behavior. Rejected.

## Consequences (amended 2026-10-27 per T-112 review)

- Tier-2 shim suite (`make test-shim`) runs in PR CI as its own job; the
  conformance harness joins post-M2 when the adapter exists to exercise.

- `test/fakeexec/` and the `dokku` shim land in Milestone 1 with the exec
  package, so all later milestones inherit the seam.
- The Lima template + cloud-init are public artifacts and must stay
  generic (no private registry pulls, no private repos).
- Acceptance criteria in milestone docs cite which tier verifies them.

## Open questions

- Lima vs Multipass: **resolved by owner decision (M0 review answer #5)— Lima.**
- Tier-3 on macOS CI runners: skip; Linux-only.
