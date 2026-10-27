package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

func TestRecordReconcileRun(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	good := domain.Ref("default", "app-a")
	bad := domain.Ref("default", "app-b")

	op := mustInsertOp(t, st, "delivery-r1", domain.OpReconcile)
	run := &domain.ReconciliationRun{
		OperationID:   op.ID,
		Actor:         domain.ActorWebhook,
		CorrelationID: "delivery-r1",
		Revision:      "1a2b3c4",
		Status:        domain.RunFailed, // partial failure
		StartedAt:     time.Now().UTC(),
		FinishedAt:    time.Now().UTC().Add(time.Second).Round(time.Microsecond),
		Outcomes: []domain.ServiceOutcome{
			{Ref: good, Result: domain.OutcomeSucceeded, Changes: []string{"revision none→1a2b3c4"}, DurationMs: 100},
			{Ref: bad, Result: domain.OutcomeFailed, Err: "backend: build failed (redacted)"},
		},
	}
	cp := &store.Checkpoint{
		LastApplied:             run.Revision, // advanced even on partial failure
		LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{},
		AppliedPerService:       map[domain.ServiceRef]string{},
	}
	if err := st.RecordReconcileRun(ctx, run, cp); err != nil {
		t.Fatalf("RecordReconcileRun: %v", err)
	}

	// Atomic composite: checkpoint written in the same transaction as the
	// run, and per-service applied revisions advanced for the applying ones.
	got, err := st.GetCheckpoint(ctx)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if got.LastApplied != "1a2b3c4" || got.LastKnownGood != "" {
		t.Fatalf("checkpoint after partial run: %+v (LastKnownGood must not advance)", got)
	}
	if got.AppliedPerService[good] != "1a2b3c4" || got.AppliedPerService[bad] != "" {
		t.Fatalf("applied map after partial run: %+v", got.AppliedPerService)
	}

	// A fully-green run advances everything.
	op2 := mustInsertOp(t, st, "delivery-r2", domain.OpReconcile)
	run2 := &domain.ReconciliationRun{
		OperationID:   op2.ID,
		Actor:         domain.ActorController,
		CorrelationID: "delivery-r2",
		Revision:      "b8c9d0e",
		Status:        domain.RunSucceeded,
		StartedAt:     time.Now().UTC(),
		Outcomes: []domain.ServiceOutcome{
			{Ref: good, Result: domain.OutcomeNoop},
			{Ref: bad, Result: domain.OutcomeSucceeded, DurationMs: 50},
		},
	}
	if err := st.RecordReconcileRun(ctx, run2, &store.Checkpoint{
		LastApplied:             run2.Revision,
		LastKnownGood:           run2.Revision,
		LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{},
		AppliedPerService:       map[domain.ServiceRef]string{},
	}); err != nil {
		t.Fatalf("RecordReconcileRun 2: %v", err)
	}
	got, _ = st.GetCheckpoint(ctx)
	if got.LastKnownGood != "b8c9d0e" || got.AppliedPerService[good] != "b8c9d0e" {
		t.Fatalf("checkpoint after green run: %+v", got)
	}

	// Atomicity: an invalid outcome rolls back the run row entirely — no
	// orphaned run or half-advanced checkpoint.
	op3, _ := domain.NewOperation(domain.OpReconcile, domain.ActorUser, "delivery-r3", "")
	if err := st.InsertOperation(ctx, op3); err != nil {
		t.Fatalf("insert op3: %v", err)
	}
	badRun := &domain.ReconciliationRun{
		OperationID: op3.ID,
		Actor:       domain.ActorUser,
		Revision:    "c0ffee1",
		Status:      domain.RunSucceeded,
		StartedAt:   time.Now().UTC(),
		Outcomes: []domain.ServiceOutcome{
			{Ref: good, Result: domain.OutcomeFailed, Err: ""}, // invalid: empty error
		},
	}
	before, _ := st.GetCheckpoint(ctx)
	if err := st.RecordReconcileRun(ctx, badRun, nil); err == nil {
		t.Fatal("invalid outcome accepted")
	}
	after := mustGetCheckpoint(ctx, t, st)
	if after.LastApplied != before.LastApplied || after.LastKnownGood != before.LastKnownGood {
		t.Fatalf("failed run mutated checkpoint: before=%+v after=%+v", before, after)
	}
	// The failed composite write left no run row for op3.
	var n int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE operation_id = ?`, op3.ID.String()).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("failed run left %d rows in runs", n)
	}
}

// mustGetCheckpoint is GetCheckpoint with test-provided fatal context.
func mustGetCheckpoint(ctx context.Context, t *testing.T, st *Store) *store.Checkpoint {
	t.Helper()
	cp, err := st.GetCheckpoint(ctx)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	return cp
}

func TestRecordReconcileRunValidation(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	id, _ := domain.NewOperation(domain.OpReconcile, domain.ActorUser, "c", "")
	if err := st.RecordReconcileRun(ctx, &domain.ReconciliationRun{Revision: "a", Status: "what"}, nil); errOf(err, domain.ErrInvalid) {
	} else {
		t.Fatalf("bad status err = %v, want ErrInvalid", err)
	}
	if err := st.RecordReconcileRun(ctx, &domain.ReconciliationRun{Revision: "a", Status: domain.RunSucceeded}, nil); errOf(err, domain.ErrInvalid) {
	} else {
		t.Fatalf("zero op id err = %v, want ErrInvalid", err)
	}
	op := mustInsertOp(t, st, "c", domain.OpReconcile)
	_ = id
	if err := st.RecordReconcileRun(ctx, &domain.ReconciliationRun{
		OperationID: op.ID,
		Status:      domain.RunSucceeded,
		Outcomes:    []domain.ServiceOutcome{{Result: "bogus"}},
	}, nil); errOf(err, domain.ErrInvalid) {
	} else {
		t.Fatalf("bogus outcome err = %v, want ErrInvalid", err)
	}
}

func TestBusyTimeoutIsApplied(t *testing.T) {
	// The DSN carries the ADR-0007 pragmas; verify they really took effect
	// on the pooled connection (journal_mode, busy_timeout readable back).
	st := openTest(t)
	ctx := context.Background()
	for q, want := range map[string]string{
		"PRAGMA journal_mode": "wal",
		"PRAGMA foreign_keys": "1",
		"PRAGMA busy_timeout": "5000",
	} {
		var got string
		if err := st.db.QueryRowContext(ctx, q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", q, got, want)
		}
	}
}
