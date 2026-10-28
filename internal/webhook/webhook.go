// SPDX-License-Identifier: MIT

// Package webhook implements TOP's Git webhook intake endpoint (T-303,
// ADR-0003): a net/http Handler, mountable on the controller's HTTP server
// (next wave), that verifies GitHub-style HMAC-SHA256 signatures, applies
// the event allowlist, dedups deliveries through the StateStore, and
// enqueues a reconcile via a caller-provided Enqueuer.
//
// Hard security rules (ADR-0003 + docs/contracts/git-source.md):
//
//   - the HMAC secret comes from the SecretProvider seam (ADR-0008) — it is
//     never logged, never placed on argv, and never stored in this package;
//   - nothing of a payload is ever executed or used as desired state: the
//     only parsed fields are the `ref` (audit/log only) — the reconciler
//     always re-derives everything from the configured remote;
//   - a failed signature check never touches the store and never enqueues.
//
// Webhooks are a *hardening layer* only: the poll + periodic loops remain
// the correctness backstop (ADR-0003), so every failure mode here
// degrades to "poll will catch it".
package webhook

import (
	"context"
	"time"
)

// DefaultSecretName is the secret name this handler resolves through the
// SecretProvider for the webhook HMAC secret (ADR-0008: values live in the
// host secret store, e.g. /var/lib/top/secrets/webhook-hmac).
const DefaultSecretName = "webhook-hmac"

// DefaultMaxBodyBytes is the request-body cap when Options.MaxBodyBytes is
// zero (64 KiB — GitHub push payloads for a platform spec repo are far
// smaller; the contract leaves the number to the implementation).
const DefaultMaxBodyBytes = 64 << 10

// Event carries the minimal facts extracted from a verified delivery. It is
// all the enqueue seam sees: no payload bytes, no parsed JSON, nothing
// executable — the reconciler re-fetches from the configured remote
// (ADR-0003 "payload content never used as desired state").
type Event struct {
	// DeliveryID is the provider delivery identifier (X-GitHub-Delivery);
	// also the correlation id recorded with the reconcile operation
	// (docs/contracts/README.md actor/correlationId discipline).
	DeliveryID string
	// Event is the provider event name (X-GitHub-Event), e.g. "push";
	// guaranteed to be in the allowlist by the time Event is produced.
	Event string
	// Ref is the parsed git ref from the payload body (e.g. "refs/heads/main"),
	// for audit logging only. Never a command argument; never fetch targets.
	Ref string
}

// Enqueuer is the seam between webhook intake and the controller daemon's
// reconcile queue (next wave). internal/webhook never imports
// internal/controller; the controller implements this (typically a
// coalescing buffered channel per ADR-0003 queue semantics).
//
// Enqueue must not block indefinitely: intake returns 202 only when the
// delivery has been handed off.
type Enqueuer interface {
	// Enqueue hands one verified delivery to the reconcile queue. A non-nil
	// error means the delivery was NOT accepted (handler responds 503 so
	// the provider retries later).
	Enqueue(ctx context.Context, ev Event) error
}

// SecretProvider is the ADR-0008 SecretProvider seam, stated locally as a
// structural interface so this package does not import internal/secrets
// (internal/secrets.HostFile satisfies it).
type SecretProvider interface {
	// Resolve returns the secret value and version for name.
	Resolve(ctx context.Context, name string) (value string, version string, err error)
}

// Deduper is the delivery-dedup seam backed by the StateStore
// (docs/contracts/state-store.md: deliveries(deliveryID PK, receivedAt);
// store.Store.RecordDelivery satisfies it). true = first sighting, so the
// delivery should be processed; false = duplicate, already handled.
type Deduper interface {
	RecordDelivery(ctx context.Context, deliveryID string, at time.Time) (bool, error)
}

// Result labels for obs.Metrics.RecordWebhookDelivery. They are a closed
// set so the metric's `result` label cardinality stays bounded
// (ADR-0010): raw event names never flow into the label for untrusted
// deliveries — only allowlisted event names, resultEventOther, or
// resultEventInvalid.
const (
	// ResultAccepted: verified, allowlisted, deduped as first-seen, enqueued.
	ResultAccepted = "accepted"
	// ResultDuplicate: verified but a known delivery id — 200, no enqueue.
	ResultDuplicate = "duplicate"
	// ResultIgnored: verified but the event is not on the allowlist — 202,
	// no enqueue (ADR-0003 open-question proposal: keep the delivery audit
	// complete; a no-op is harmless because poll is the backstop).
	ResultIgnored = "ignored"
	// ResultRejected: signature/body-size/parsing failure — 401/413/400,
	// no store write, no enqueue.
	ResultRejected = "rejected"
	// ResultError: store or enqueue failure — 500/503, no enqueue.
	ResultError = "error"
)

// Event label values used when the header's event name is NOT bounded by
// the allowlist (unknown-event and rejected-before-parse deliveries), so
// attacker-controlled header values cannot create label cardinality.
const (
	resultEventOther   = "other"
	resultEventInvalid = "invalid"
)
