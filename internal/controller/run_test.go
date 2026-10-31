// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/obs"
	"github.com/toledompm/top/internal/store"
)

// sha1/sha2 are 40-hex fakes for the tests below (gitsource sha shape).
const (
	testSHA1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testSHA2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestRunTriggerRecordsRunAndAdvancesCheckpoint exercises one full
// trigger → reconciler.Run pass on fakes: the run is recorded with the
// trigger's actor + correlation, the checkpoint advances to the resolved
// sha, the snapshot is fetched exactly once, and the metric says the run
// drifted something (first create of demo → drift green run).
func TestRunTriggerRecordsRunAndAdvancesCheckpoint(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	be := newFakeBackend()
	m, err := obs.NewMetrics("test", "nocommit")
	if err != nil {
		t.Fatal(err)
	}
	d := newTestDaemon(t, src, st, be, Options{Metrics: m})

	d.runTrigger(context.Background(), Trigger{
		Source: SourcePoll, Branch: "main", CorrelationID: "poll:" + testSHA1[:12],
	})

	runs := st.runsOf()
	if len(runs) != 1 {
		t.Fatalf("runs recorded: %d, want 1", len(runs))
	}
	r := runs[0]
	if r.Actor != domain.ActorController {
		t.Fatalf("actor got %q, want controller (poll trigger)", r.Actor)
	}
	if r.Revision != testSHA1 {
		t.Fatalf("revision got %q, want %q", r.Revision, testSHA1)
	}
	if r.Status != domain.RunSucceeded {
		t.Fatalf("status got %q, want succeeded (fake backend converges)", r.Status)
	}
	if cp := st.lastCheckpoint(); cp.LastApplied != testSHA1 {
		t.Fatalf("checkpoint LastApplied got %q, want %q", cp.LastApplied, testSHA1)
	}
	if got, want := len(src.fetches), 1; got != want {
		t.Fatalf("fetches got %d, want %d", got, want)
	}

	if got := countMetric(t, m, "top_reconcile_total", "result", obs.ResultDrift); got != 1 {
		t.Fatalf("reconcile_total{result=drift} got %v, want 1", got)
	}
}

// TestRunTriggerLeaseConflictRecordsNoHistory pins the honored reading of
// the reconciler's open question: a lease-blocked run appends NO history
// row and mutates nothing — the daemon only logs it and continues.
func TestRunTriggerLeaseConflictRecordsNoHistory(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	st.leaseErr = domain.ErrLocked
	be := newFakeBackend()
	d := newTestDaemon(t, src, st, be, Options{})

	d.runTrigger(context.Background(), Trigger{Source: SourcePoll, Branch: "main"})

	if runs := st.runsOf(); len(runs) != 0 {
		t.Fatalf("lease-blocked run recorded %d history rows, want 0", len(runs))
	}
	if len(st.ops) != 0 {
		t.Fatalf("lease-blocked run wrote %d operation rows, want 0", len(st.ops))
	}
}

// TestRunTriggerFailureIsContained pins the pump's calm-face behavior: a
// failed deploy still records the run (actor webhook), and the pump
// simply carries on.
func TestRunTriggerFailureIsContained(t *testing.T) {
	src := &fakeSource{shas: []string{testSHA1}}
	st := newFakeStore()
	be := newFakeBackend()
	be.failDeploy = domain.ErrUnhealthy
	d := newTestDaemon(t, src, st, be, Options{})

	d.runTrigger(context.Background(), Trigger{Source: SourceWebhook, Branch: "main", CorrelationID: "delivery-1"})

	runs := st.runsOf()
	if len(runs) != 1 {
		t.Fatalf("runs: %d, want 1", len(runs))
	}
	if runs[0].Status != domain.RunFailed {
		t.Fatalf("status got %q, want failed", runs[0].Status)
	}
	if runs[0].Actor != domain.ActorWebhook {
		t.Fatalf("actor got %q, want webhook", runs[0].Actor)
	}
}

// TestPollOnce drives single cycles of the poll loop without tickers:
//   - fresh checkpoint (no state store row) → the INITIAL enqueue
//     (fresh-install semantics, documented in the README),
//   - checkpoint sha == resolved sha → no trigger,
//   - moved sha → exactly one trigger.
func TestPollOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh checkpoint triggers initial run", func(t *testing.T) {
		src := &fakeSource{shas: []string{testSHA1}}
		st := newFakeStore()
		be := newFakeBackend()
		d := newTestDaemon(t, src, st, be, Options{})
		d.pollOnce(ctx, time.Now())
		if got, want := d.queue.PendingCount(), 1; got != want {
			t.Fatalf("pending got %d, want %d", got, want)
		}
	})

	t.Run("unchanged sha does not enqueue", func(t *testing.T) {
		src := &fakeSource{shas: []string{testSHA1}}
		st := newFakeStore()
		be := newFakeBackend()
		d := newTestDaemon(t, src, st, be, Options{})
		st.cps = append(st.cps, store.Checkpoint{LastApplied: testSHA1})

		d.pollOnce(ctx, time.Now())
		if got, want := d.queue.PendingCount(), 0; got != want {
			t.Fatalf("pending got %d, want %d", got, want)
		}
	})

	t.Run("changed sha enqueues exactly one", func(t *testing.T) {
		src := &fakeSource{shas: []string{testSHA2}}
		st := newFakeStore()
		be := newFakeBackend()
		d := newTestDaemon(t, src, st, be, Options{})
		st.cps = append(st.cps, store.Checkpoint{LastApplied: testSHA1})

		d.pollOnce(ctx, time.Now())
		d.pollOnce(ctx, time.Now()) // reconcile-side coalescing second layer
		if got, want := d.queue.PendingCount(), 1; got != want {
			t.Fatalf("pending got %d, want %d", got, want)
		}
	})
}

// TestPeriodicLoopEnqueueShape: one schedule trigger keeps identity
// (schedule, branch). Driven by calling the trigger-build inline — the
// ticker pacing itself is trivial and the full loop is exercised in
// daemon_test via Run.
func TestPeriodicTriggerShape(t *testing.T) {
	if (Trigger{Source: SourceSchedule, Branch: "main"}).ID() != "schedule|main" {
		t.Fatal("schedule trigger identity lost")
	}
}
