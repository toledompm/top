# TOP controller daemon (internal/controller)

T-302 / T-310 — ADR-0003 "git sync: webhooks + polling": the daemon that turns
triggers into reconcile runs.

## Loop architecture

Three producers, one queue, one pump:

- **Webhook intake** (only when `Addr` is set): internal/webhook's handler is
  mounted at `/webhooks/github` (verified, delivery-id-deduped), mapped to a
  `(webhook, branch)` trigger.
- **Poll loop**: every `PollInterval` (ADR-0003 canon: 60 s) resolves the
  configured branch with `gitsource.Source.ResolveLatest`, compares it to the
  store checkpoint's `LastApplied`, and enqueues one `(poll, branch)` trigger
  on movement. The poll is the missed-webhook backstop.
- **Periodic sweep**: every `ReconcileInterval` (ADR-0003 canon: 1 h) enqueues
  a `(schedule, branch)` trigger regardless of sha state — it is what catches
  out-of-band host changes.

All three enqueue into one coalescing `Queue` keyed by the trigger identity
`(source, branch)`. **Coalescing semantics:** while an identity is already
pending, every further arrival of that identity is absorbed (a counter only —
the run re-resolves the repo sha at its own start, so the newest desired state
always wins). The pump dequeues one identity at a time → one full
`reconcile.Reconciler.Run` (resolve → fetch snapshot → spec.LoadDir → Run:
validate, lease, order, diff, apply, gate, record) → record the outcome →
next. A trigger failing NEVER stops the pump. Proof test:
`TestQueueTriggerStormCoalescesToSinglePending` (50 concurrent same-identity
enqueues → 1 pending, 49 absorbed).

### Fresh-store first-poll semantics (decided here)

On a fresh store the checkpoint's `LastApplied` is empty, so the first
successful resolution ALWAYS enqueues the initial run — the daemon converges
an empty host on startup without needing a webhook or a schedule tick. After
that first run the checkpoint advances, and later polls trigger only on real
sha movement. There is deliberately no "snapshot the sha without running"
shortcut: a fetch failure before a run leaves the checkpoint untouched, so the
next tick retries.

## HTTP server

`/webhooks/github` → `webhook.NewHandler` (HMAC verify → dedup → enqueue;
503-on-queue-closed so the provider retries while the poll backstops).
`/metrics` → `obs.Metrics` (ADR-0010 counters/gauges). `/healthz` → 200 while
serving, 503 "draining" from the first moment of shutdown (readiness flips
before the socket closes). Hardening: server read/header/idle timeouts,
bounded header size, handler-level body cap.

## Shutdown ordering

1. readiness OFF (load balancers drain before the intake dies),
2. in-flight HTTP requests drained (bounded `HTTPDrainTimeout`),
3. producers stop,
4. queue closed — pending triggers discarded, nothing new scheduled,
5. the ACTIVE run gets `DrainGrace` (default 30 s, bounded per ADR-0005) to
   reach a recorded terminal state; past that it is cancelled, and the
   reconciler releases the lease and persists with `WithoutCancel` either way,
   then the process exits cleanly. A run lease-blocked by another actor
   (`topctl reconcile --now`) appends NO history row — the daemon logs and
   moves on (reconcile's documented open question, honored here).

## Configuration

`Options` (built via `OptionsFromConfig(config.Config)` + CLI-provided seams:
Reconciler, Store, Source, Secret, Log): `controller.poll_interval_seconds`,
`controller.reconcile_interval_seconds`, `git.branch`, `data_dir` (volume
root) come from config; pacing env `TOP_CONTROLLER_POLL_INTERVAL_MS`
(optional; valid values change ONLY polling cadence, invalid values are
ignored — mirroring internal/install's knob precedent).

The production entrypoint is:

```go
func Serve(o Options) error
```

signal-aware wrapper around `Daemon.Run(ctx)`: returns nil on SIGINT/SIGTERM,
the fatal server error otherwise. The `topctl controller run` command builds
`OptionsFromConfig`, fills `Reconciler` (reconcile.New), `Store` (sqlite),
`Source` (gitsource.New), `Secret` (webhook-hmac SecretProvider, ADR-0008) and
calls `Serve`.
