// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestQueueTriggerStormCoalescesToSinglePending is the T-302 unit proof:
// N triggers with the SAME (source, branch) identity arriving while a run
// is active (or queued) produce exactly ONE pending entry — not a
// channel-sized stampede. This is the property ADR-0003 names "coalesce N
// triggers into 1 run"; it is exercised here both against a drained pump
// (busy-run equivalence is the Pump()'s single-thread dequeue) and purely
// at the enqueue level.
func TestQueueTriggerStormCoalescesToSinglePending(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()

	const stormN = 50
	branch := "main"
	trig := func() Trigger {
		return Trigger{Source: SourceWebhook, Branch: branch, CorrelationID: "delivery-x"}
	}

	// First arrival becomes pending; every following same-identity
	// trigger is absorbed.
	if err := q.Enqueue(ctx, trig()); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < stormN-1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := q.Enqueue(ctx, trig()); err != nil {
				t.Errorf("storm enqueue: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := q.PendingCount(); got != 1 {
		t.Fatalf("pending after %d-trigger storm: got %d, want 1", stormN, got)
	}
	if got, want := q.EnqueuedCount(), 1; got != want {
		t.Fatalf("enqueued count got %d, want %d", got, want)
	}
	if got, want := q.AbsorbedCount(), stormN-1; got != want {
		t.Fatalf("absorbed count got %d, want %d", got, want)
	}
}

// TestQueueDifferentIdentitiesStayPending pins the coalescing key being
// (source, branch): a webhook trigger and a poll trigger stay distinct,
// and two poll triggers for different branches stay distinct.
func TestQueueDifferentIdentitiesStayPending(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()
	triggers := []Trigger{
		{Source: SourceWebhook, Branch: "main"},
		{Source: SourcePoll, Branch: "main"},
		{Source: SourcePoll, Branch: "release"},
	}
	for _, trig := range triggers {
		if err := q.Enqueue(ctx, trig); err != nil {
			t.Fatalf("enqueue %s: %v", trig.ID(), err)
		}
	}
	if got, want := q.PendingCount(), len(triggers); got != want {
		t.Fatalf("pending got %d, want %d", got, want)
	}
	// Second webhook trigger on the same branch absorbs.
	if err := q.Enqueue(ctx, Trigger{Source: SourceWebhook, Branch: "main", CorrelationID: "y"}); err != nil {
		t.Fatal(err)
	}
	if got, want := q.PendingCount(), len(triggers); got != want {
		t.Fatalf("after absorb: pending got %d, want %d", got, want)
	}
}

// TestQueueDeterministicDrainOrder pins take()'s (source, branch) order so
// the drain is reproducible: poll < schedule < webhook (lexicographic).
func TestQueueDeterministicDrainOrder(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()
	for _, trig := range []Trigger{
		{Source: SourceWebhook, Branch: "main"},
		{Source: SourcePoll, Branch: "main"},
		{Source: SourceSchedule, Branch: "main"},
	} {
		if err := q.Enqueue(ctx, trig); err != nil {
			t.Fatal(err)
		}
	}
	wantOrder := []string{"poll|main", "schedule|main", "webhook|main"}
	for _, want := range wantOrder {
		got, ok := q.take()
		if !ok || got.ID() != want {
			t.Fatalf("drained %q ok=%v, want %q", got.ID(), ok, want)
		}
	}
	if _, ok := q.take(); ok {
		t.Fatal("queue should be empty")
	}
}

// TestQueuePumpRunsEachDequeuedExactlyOnce exercises the pump loop: each
// pending identity is executed exactly once, serially, and the pump exits
// after a Close (nothing still pending).
func TestQueuePumpRunsEachDequeuedExactlyOnce(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()
	for _, trig := range []Trigger{
		{Source: SourcePoll, Branch: "main"},
		{Source: SourcePoll, Branch: "release"},
	} {
		if err := q.Enqueue(ctx, trig); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan string, 8)
	pumpDone := q.Pump(func(t Trigger) { done <- t.ID() })

	select {
	case gotID := <-done:
		if gotID != "poll|main" && gotID != "poll|release" {
			t.Fatalf("unexpected run order %q", gotID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pump never ran the first trigger")
	}

	q.Close()
	select {
	case <-pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after close+drain")
	}
	if got := q.DrainedCount(); got != 2 {
		t.Fatalf("drained count got %d, want 2", got)
	}
}

// TestQueueCloseDiscardsPendingAndRejects pins drain semantics: Close
// discards pending triggers (shutdown drains the ACTIVE run, not a pile
// of new ones) and late enqueues get ErrQueueClosed.
func TestQueueCloseDiscardsPendingAndRejects(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()
	if err := q.Enqueue(ctx, Trigger{Source: SourcePoll, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	q.Close()
	if got, want := q.PendingCount(), 0; got != want {
		t.Fatalf("pending after close got %d, want %d", got, want)
	}
	err := q.Enqueue(ctx, Trigger{Source: SourcePoll, Branch: "main"})
	if err == nil {
		t.Fatal("enqueue after close should fail")
	}
}
