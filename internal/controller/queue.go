// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Trigger sources — ADR-0003's "three triggers, one queue". Manual runs
// (`topctl reconcile --now`) bypass this queue; they meet the daemon only
// at the store's reconcile lease (one reconcile at a time, in-process and
// cross-process).
const (
	// SourcePoll: a poll cycle saw the branch sha move (or a fresh
	// checkpoint needing an initial run — see the README's checkpoint
	// semantics).
	SourcePoll = "poll"
	// SourceSchedule: the periodic full-reconcile safety sweep went off.
	SourceSchedule = "schedule"
	// SourceWebhook: a verified, deduped webhook delivery (intake is
	// internal/webhook; dedup already happened there — this queue's
	// coalescing is the SECOND layer behind the store's delivery id).
	SourceWebhook = "webhook"
)

// Trigger is ONE reason to reconcile. Its identity is (Source, Branch):
// coalescing keys on that pair, so a storm of deliveries and poll ticks
// while a run is queued or executing collapses into a single pending
// entry — the run resolves the repo sha at ITS own start time (ADR-0003),
// so the newest desired state always wins regardless of which trigger
// survived coalescing.
type Trigger struct {
	// Source is one of the Source* constants above.
	Source string
	// Branch is the trigger branch (the configured platform-repo branch
	// for all three sources — webhook deliveries re-derive desired state
	// from the configured remote, never from the payload).
	Branch string
	// CorrelationID ties the resulting operation to its trigger
	// (webhook: the delivery id; poll: "poll:<sha-prefix>"; schedule: a
	// time stamp). Coalesced-away duplicates keep the FIRST arriving
	// correlation id; the operation record carries what it carries.
	CorrelationID string
}

// ID is the coalescing key: (source, branch). There is deliberately
// nothing else in it — not the sha (unknown at webhook time), not the
// delivery id (dedup is the FIRST layer, upstream in internal/webhook).
func (t Trigger) ID() string { return t.Source + "|" + t.Branch }

// ErrQueueClosed is returned by Enqueue after Close: shutdown-drain
// semantics — intake (a late HTTP delivery during server drain) learns
// the queue is no longer accepting and answers upstream accordingly.
var ErrQueueClosed = errors.New("controller: trigger queue is closed")

// Queue is ADR-0003's queue semantics: pending-set dedup (coalesce N
// triggers into 1 run) with one run executing at a time. Nothing here
// schedules — the dequeuing side is Pump, wired to the reconciler by the
// daemon; the enqueueing side is safe for concurrent use by the poll
// loop, the periodic loop, and the webhook handler.
//
// Coalescing is second-layer dedup behind the store's per-delivery id
// (docs/contracts/state-store.md, deliveries table): two distinct, valid
// deliveries of the same push still coalesce here.
type Queue struct {
	mu      sync.Mutex
	pending map[string]Trigger
	// notify is a capacity-one "pending may have changed" signal. It
	// carries no data; readers always re-check take().
	notify chan struct{}
	closed bool

	// Counters for the coalescing proof and daemon logging.
	enqueued int
	absorbed int
	drained  int
}

// NewQueue builds an empty, open queue.
func NewQueue() *Queue {
	return &Queue{pending: map[string]Trigger{}, notify: make(chan struct{}, 1)}
}

// Enqueue offers one trigger. A trigger whose ID is already pending is
// ABSORBED (counter only — the already-pending one suffices; the run
// re-resolves state at start). Enqueue never blocks on queue state; it
// only fails when the queue is closed (drain phase).
func (q *Queue) Enqueue(ctx context.Context, t Trigger) error {
	_ = ctx // the queue is memory-only; upstreams time out instead
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	if _, dup := q.pending[t.ID()]; dup {
		q.absorbed++
		return nil
	}
	q.pending[t.ID()] = t
	q.enqueued++
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return nil
}

// take pops exactly one pending trigger in deterministic (source, branch)
// order so the proof test and any future logging stay reproducible.
func (q *Queue) take() (Trigger, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return Trigger{}, false
	}
	keys := make([]string, 0, len(q.pending))
	for k := range q.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t := q.pending[keys[0]]
	delete(q.pending, keys[0])
	q.drained++
	return t, true
}

// isOpen reports the queue still accepts (matters to Pump's exit rule).
func (q *Queue) isOpen() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.closed
}

// Close stops accepting triggers and DISCARDS whatever is still pending:
// shutdown drains the ACTIVE run to a terminal state (bounded, daemon
// side) — it does not schedule a pile of new runs on the way out. Closing
// wakes blocked pumps. A closed queue cannot reopen; a late Enqueue
// (should be impossible once the server drained) is a typed error.
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.pending = map[string]Trigger{}
	q.mu.Unlock()
	close(q.notify)
}

// Pump runs one triggered reconcile at a time, serially, until the queue
// is closed and fully drained. run receives every dequeued trigger; a
// non-nil error is recorded by the caller (runTrigger) — it NEVER stops
// the pump (ADR-0003: failures do not kill sync). run's context comes
// from the daemon, so shutdown can give the active run a bounded grace.
//
// Pump returns a channel closed exactly when the pump exits.
func (q *Queue) Pump(run func(t Trigger)) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			t, ok := q.take()
			if ok {
				run(t)
				continue
			}
			if !q.isOpen() {
				return // closed AND empty
			}
			<-q.notify
		}
	}()
	return done
}

// Counters (for tests and the README's proof names).

// EnqueuedCount is how many trigger identities entered the pending set.
func (q *Queue) EnqueuedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.enqueued
}

// AbsorbedCount is how many same-identity triggers were coalesced away.
func (q *Queue) AbsorbedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.absorbed
}

// DrainedCount is how many pending entries were handed to a run.
func (q *Queue) DrainedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.drained
}

// PendingCount is the current pending-set size (0 when empty/closed).
func (q *Queue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}
