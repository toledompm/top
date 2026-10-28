# internal/webhook — Git webhook intake (T-303, ADR-0003)

The controller's `POST /webhooks/github` endpoint: verify GitHub-style
HMAC-SHA256 signatures, apply the event allowlist, dedup deliveries through
the StateStore, and enqueue a reconcile. **Webhooks are a hardening layer
only** — the 60 s poll and the 1 h periodic reconcile remain the correctness
backstop (ADR-0003), so this endpoint being down never blocks convergence.

**Not mounted yet.** `internal/cli` does not host it in v0; the controller
HTTP server (T-310, next wave) mounts it:

```go
mux.Handle("/webhooks/github", h := webhook.NewHandler(webhook.Options{
    Secret:   secretsProvider,      // *secrets.HostFile satisfies the seam
    Dedup:    store,                // store.Store satisfies RecordDelivery
    Queue:    reconcileEnqueuer,    // controller daemon's coalescing queue
    Recorder: metrics,              // *obs.Metrics (optional)
    Log:      logger,
}))
```

## Request semantics (v0, fixed here so tests + docs agree)

| Case | Status | Body | Store write | Enqueue | Metric `{event,result}` |
|---|---|---|---|---|---|
| verified + allowlisted + first-seen | **202** | `{"accepted":true,"deduped":false}` | yes | yes | `{push,accepted}` |
| verified but **duplicate** delivery id | **200** | `{"accepted":false,"deduped":true}` | yes (sight 1 only) | **no** | `{push,duplicate}` |
| verified, event **not allowlisted** | **202** | `{"accepted":true,"deduped":false}` | yes | no | `{other,ignored}` |
| forged/bad signature | **401** | envelope | **no** | no | `{event,rejected}` |
| body over the cap | **413** | envelope | no | no | `{event,rejected}` |
| verified but **malformed payload** | **400** | envelope | **no** (dedup only successful parses) | no | `{push,rejected}` |
| missing `X-GitHub-Delivery` | **400** | envelope | no | no | `{event,rejected}` |
| GET / other method | **405** | envelope | no | no | `{invalid,rejected}` |
| secret resolve failed | **500** | envelope | no | no | `{invalid,error}` |
| dedup store failed | **500** | envelope | attempted | no | `{push,error}` |
| queue rejected | **503** (provider will retry) | envelope | yes | failed | `{push,error}` |

Deviations from the task brief, with reasons:

- **Duplicate → 200, not 202.** ADR-0003 states "duplicate gets 200 and no
  enqueue", and `docs/contracts/state-store.md` pins it
  (`if !store.RecordDelivery(...) { return 200, "duplicate" }`). The ADR is
  authoritative.
- **Unknown event → 202 no-op.** ADR-0003's open-question proposal (verify
  + 202 + no enqueue, "keeps the delivery audit complete"). The allowlist
  check runs *after* dedup so a retried unknown-event delivery is also
  deduped, not double-counted.
- **Dedup only successful parses.** A malformed-but-signed body is 400 with
  no store write, so a bad payload can never poison a delivery id; the
  provider's retry will deliver the corrected payload and be processed.

## Headers (GitHub convention; ADR-0003 names `sha256=` + `X-GitHub-Event`)

- `X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(secret, raw-body))>` —
  computed over the exact capped body bytes, compared with `hmac.Equal`
  (constant time; no string compare, no early-exit). Any other scheme
  (`sha1=`) is rejected, never fallen back to.
- `X-GitHub-Event` — allowlist: `push` only (Options.AllowedEvents can
  widen it for a verifier variant).
- `X-GitHub-Delivery` — the dedup key and the reconcile operation's
  correlation id; ≤ 128 bytes, required.

Body cap: `http.MaxBytesReader`, default **64 KiB**
(`Options.MaxBodyBytes`; docs do not fix a number — this default is hereby
proposed as the contract value).

## Security invariants

- The HMAC secret is resolved **per request** through the SecretProvider
  seam (ADR-0008; name defaults to `webhook-hmac`, 
  `webhook.DefaultSecretName`) so secret rotation needs no restart. It is
  never logged, never on argv, never in a response.
- **Nothing of the payload is ever executed or used as desired state**
  (ADR-0003 + git-source contract security rules): the only parsed field is
  the push `ref`, kept in `Event.Ref` for *audit logging* — it is never an
  exec argument and the reconciler always re-derives the branch/sha from
  the configured remote. `Event` is purposely shaped so payload content
  cannot join it (pinned by a test).
- Cardinality discipline (ADR-0010): metrics use a closed result set
  (`accepted|duplicate|ignored|rejected|error`) and bounded event labels —
  untrusted header values map to `other`/`invalid`, never raw.

## Metrics

No new metrics were needed: `obs.Metrics.RecordWebhookDelivery(event, result)`
(`top_webhook_deliveries_total`, ADR-0010) already existed and is the only
surface — this package increments it truthfully for every delivery via the
narrow `webhook.Recorder` interface (`*obs.Metrics` satisfies it; nil = no
metrics in tests).

## Store

Delivery dedup uses the **existing** `store.Store.RecordDelivery(ctx, id, at)
(bool, error)` — the `deliveries(deliveryID PK, received_at)` table and its
INSERT-or-IGNORE semantics. **No schema change, no migration was needed.**
There is no TTL/cleanup: records live forever (a delivery id namespace is
provider-unique, so unbounded growth at homelab volume is a non-issue; if a
window is ever wanted, it is a store-side change, flagged for the store
owner — not done here).

## Testing

- Tier-1 table test (`webhook_test.go`): valid / forged / unknown-event /
  too-big / malformed / missing-delivery / wrong-scheme / 405 / 500s, plus
  the exact 202/200 envelope shapes, single-enqueue-across-duplicates, and
  metric increments.
- Signature strictness test: every single-byte flip of a valid signature
  fails (evidence the check is exact-match via `hmac.Equal`, both keys and
  bodies cross-checked).
- Real-store dedup (`store_test.go`): `internal/store/sqlite.Open` on a
  temp-file DB, including a **reopen** proving dedup durability across
  controller restarts. No `tier3` gating needed — no Dokku involved.

Handoff notes for T-310 (controller daemon): build an `Enqueuer` over the
ADR-0003 queue (buffered channel + pending-set coalescing), mount the
handler on `/webhooks/github`, and provision the `webhook-hmac` secret at
install time (ADR-0008) — `topctl` will need a documented path for the
operator to fetch the value to paste into the Git provider.
