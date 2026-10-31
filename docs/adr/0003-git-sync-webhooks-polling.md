# 0003 — Git sync: webhooks, polling, retries, concurrency

- Status: **accepted** (2026-10-27, owner approval)
- Date: 2026-10-27
- Needs explicit approval: no

## Context

The brief: webhooks trigger reconciliation after Git changes; a periodic loop
(default 1h) is the backstop; manual reconcile exists; missed/duplicated/
out-of-order deliveries must not corrupt anything; payloads must be
authenticated and never executed.

## Decision

**Three triggers, one queue:**

1. **Webhook** — `POST /webhooks/github` on the controller's HTTP server.
   Verification: HMAC-SHA256 over the raw body against a shared secret
   (host secret store, ADR-0008), `hmac.Equal` on the `sha256=` signature,
   `X-GitHub-Event` allowlist (`push` only in v0), body capped
   (`http.MaxBytesReader`). On success: record delivery ID
   (`INSERT OR IGNORE` → duplicate gets 200 and no enqueue), enqueue a
   reconcile, respond 202. The payload is parsed for repo/ref/sha *for
   logging only* — the reconciler always re-fetches from the configured
   remote; **no content of any payload is ever executed or used as desired
   state**, which neutralizes forged-but-signed and replayed content alike.
2. **Poll** — `git ls-remote` every 60 s (configurable): cheap sha compare;
   mismatch enqueues a reconcile. This is the missed-webhook repair path and
   the mutable-tag move detector.
3. **Periodic full reconcile** — default **1 h** (configurable): the drift
   sweep and the guarantee that out-of-band host changes get corrected.

**Queue semantics**: single buffered channel + pending-set dedup (coalesce N
triggers into 1 run); a run that starts *after* a trigger resolves the repo
sha *at start time*, so out-of-order deliveries are harmless — the newest
fetch always wins.

**Concurrency**: one reconcile at a time, enforced in-process (mutex) and
cross-process (SQLite lease, ADR-0007) so a CLI `topctl reconcile --now` and the
controller can't interleave Dokku mutations. Second trigger during a run =
coalesced follow-up run.

**Retry/backoff**: transient failures (network fetch, dokku lock contention,
registry timeout) retry with capped exponential backoff (5 tries, ~30 s cap)
inside the operation; exhausted retries fail the operation, keep last
known-good running, and wait for the next trigger. Permanent errors
(validation, unknown fields, missing secret) fail fast without retry.

**Git outage behavior**: fetch failure = run aborted *before* any mutation
(desired state unresolved → nothing to converge to); observed state and
workloads untouched. Documented and tested.

**Provider scope**: v0 verifies GitHub webhook signatures; the endpoint and
verifier are isolated behind a tiny interface so Gitea/GitLab HMAC variants
are additive. Generic git servers without webhooks use poll-only, which is
fully supported.

## Alternatives considered

- **Polling only** — simplest, but webhook-driven responsiveness is a stated
  product goal; the HMAC endpoint is ~60 lines of stdlib.
- **Webhook library (go-playground/webhooks)** — stale maintenance for a
  60-line stdlib job. Rejected.
- **Per-service fine-grained reconcile from webhook paths** — premature;
  whole-repo runs at homelab scale are seconds.

## Consequences

- The webhook endpoint needs network reachability from the Git provider —
  an *environment* concern with documented options (public URL, tunnel,
  or poll-only). TOP never requires it (ADR-0014 keeps specifics local).
- Metrics cover deliveries by result, poll duration/result, and time since
  last successful reconcile (ADR-0010).
- `topctl history` shows actor=`webhook|poll|manual|schedule` per operation.

## Open questions

- Should `push` events to non-configured branches enqueue a cheap no-op or be
  rejected at intake? Proposal: verify + 202 + no-op after fetch (keeps
  delivery audit complete). Decide in M3.

## Amendment (2026-10-31, M3 conformance pass; status unchanged — accepted)

Webhook intake is now implemented (`internal/webhook`, controller HTTP
server T-310). The values fixed in code so tests + docs agree:

- **Pinned header set:**
  - `X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, raw-body))>` —
    computed over the exact capped body bytes; any other scheme (`sha1=`)
    is rejected, never fallen back to; compared with `hmac.Equal`
    (constant time).
  - `X-GitHub-Delivery` — the dedup key and the reconcile operation's
    correlation id; required, ≤ 128 bytes.
  - `X-GitHub-Event` — allowlist: `push` only in v0
    (`webhook.Options.AllowedEvents` can widen it for verifier variants).
- **Body cap:** `http.MaxBytesReader`, default **64 KiB**
  (`webhook.DefaultMaxBodyBytes` / `Options.MaxBodyBytes`). Over-cap → 413,
  no store write, no enqueue.
- **Status code pin:** first-seen valid push → **202**
  `{"accepted":true,"deduped":false}` + enqueue; duplicate delivery id →
  **200** `{"accepted":false,"deduped":true}`, no second enqueue. Signed
  malformed payload → 400 with NO store write (dedup only successful
  parses, so a bad payload can never poison a delivery id — the provider's
  retry delivers the corrected payload). Queue closed/rejected → 503 so the
  provider retries while the poll backstops.
- **Open question above, decided in M3:** non-allowlisted events are 202
  no-ops without enqueue — the allowlist check runs *after* dedup, so a
  retried unknown-event delivery is also deduped (keeps ADR-0003's
  "delivery audit complete" proposal; verified in handler code).
- **Secret resolution:** the HMAC secret is resolved **per request**
  through the SecretProvider seam (ADR-0008), secret name defaulting to
  `webhook-hmac` — rotation needs no controller restart. The value is
  never logged, never on argv, never in a response.
- **Delivery retention:** records kept forever — no TTL/cleanup (the
  delivery-id namespace is provider-unique; homelab volume makes
  unbounded growth a non-issue; a window, if ever wanted, is a store-side
  change). Reopen-persistence across controller restarts is tested against
  real SQLite.
- Payload content rule unchanged and enforced: only the push `ref` is
  parsed, for audit logging only; payload content never joins the enqueue
  (pinned by a test).
