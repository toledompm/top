//go:build conformance

package conformance_check

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // driver for seeding the downgrade probe

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// TestStoreLifecycleProbe pins docs/contracts/state-store.md's
// responsibilities, error semantics, and migration expectations on the real
// SQLite implementation, end-to-end over one temp database:
//
//   - open runs embedded migrations (fresh db usable out of the box);
//   - operations are append-only with domain.CanTransition lifecycle and
//     persisted-state checking (T-106: terminal states are locked in);
//   - RecordReconcileRun is all-or-nothing — a failed composite write leaves
//     zero residue in the checkpoint (the contract's ReconcileRun sketch);
//   - webhook delivery dedup is INSERT-OR-IGNORE semantics;
//   - the serialization lease acquires/renews/expires/releases, and a
//     foreign release is ErrLocked carrying the holder;
//   - downgrade protection: a newer goose version on disk refuses to open
//     with ErrSchemaNewer (never a silent downgrade).
func TestStoreLifecycleProbe(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open fresh db (migrate): %v", err)
	}
	defer st.Close() //nolint:errcheck // probe cleanup

	if v, err := st.SchemaVersion(ctx); err != nil || v < 1 {
		t.Fatalf("SchemaVersion on migrated db = %d, err=%v; contract: store reports its schema version", v, err)
	}

	// --- operations: append-only + lifecycle enforcement -------------------
	op, operr := domain.NewOperation(domain.OpReconcile, domain.ActorWebhook, "gh-delivery-t111", "a1b2c3")
	if operr != nil {
		t.Fatalf("NewOperation: %v", operr)
	}
	now := time.Now().UTC()
	if err := st.InsertOperation(ctx, op); err != nil {
		t.Fatalf("InsertOperation: %v", err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", now); err != nil {
		t.Fatalf("pending→running: %v", err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", now); err != nil {
		t.Fatalf("running→succeeded: %v", err)
	}
	// Terminal is terminal — and this is checked against *persisted* state,
	// not just the in-memory machine (T-106).
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", now); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("succeeded→running error = %v, want ErrConflict", err)
	}
	skips := domain.CanTransition(domain.OpPending, domain.OpSucceeded)
	if skips {
		t.Fatal("domain.CanTransition allowed pending→succeeded; contract lifecycle forbids skips")
	}

	// --- observed state round-trip ------------------------------------------
	ref := domain.Ref("core", "top-controller")
	if err := st.UpsertObserved(ctx, ref, &domain.ObservedState{
		AppName: "core-top-controller", Deployed: true, Running: true,
	}, "a1b2c3", now); err != nil {
		t.Fatalf("UpsertObserved: %v", err)
	}
	if _, rev, _, err := st.GetObserved(ctx, ref); err != nil || rev != "a1b2c3" {
		t.Fatalf("GetObserved = %q err=%v, want a1b2c3/nil", rev, err)
	}

	// --- reconcile run: atomic composite write -------------------------------
	run := &domain.ReconciliationRun{
		OperationID:   op.ID,
		Actor:         domain.ActorWebhook,
		CorrelationID: "gh-delivery-t111",
		Revision:      "a1b2c3",
		Status:        domain.RunSucceeded,
		StartedAt:     now,
		FinishedAt:    now,
		Outcomes: []domain.ServiceOutcome{{
			Ref: ref, Result: domain.OutcomeSucceeded, Changes: []string{"revision →a1b2c3"},
			DurationMs: 42100,
		}},
	}
	cp := &store.Checkpoint{LastApplied: "a1b2c3", LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{}, AppliedPerService: map[domain.ServiceRef]string{}}
	if err := st.RecordReconcileRun(ctx, run, cp); err != nil {
		t.Fatalf("RecordReconcileRun: %v", err)
	}
	gotCP, err := st.GetCheckpoint(ctx)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if gotCP.AppliedPerService[ref] != "a1b2c3" || gotCP.LastApplied != "a1b2c3" {
		t.Fatalf("checkpoint after green run = %+v", gotCP)
	}

	// Failed composite: duplicate outcome rows (same operation+ref) violate
	// the run_outcomes key mid-transaction. Rollback must leave ZERO
	// residue: the second run's revision must not appear anywhere.
	failed := &domain.ReconciliationRun{
		OperationID: uuid.New(),
		Actor:       domain.ActorController,
		Revision:    "9f8e7d6",
		Status:      domain.RunSucceeded,
		Outcomes: []domain.ServiceOutcome{
			{Ref: ref, Result: domain.OutcomeSucceeded},
			{Ref: ref, Result: domain.OutcomeNoop},
		},
	}
	if err := st.RecordReconcileRun(ctx, failed, &store.Checkpoint{
		LastApplied:             "9f8e7d6",
		LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{},
		AppliedPerService:       map[domain.ServiceRef]string{ref: "9f8e7d6"},
	}); err == nil {
		t.Fatal("RecordReconcileRun duplicate-outcome run = nil error, want tx failure")
	}
	gotCP, err = st.GetCheckpoint(ctx)
	if err != nil {
		t.Fatalf("GetCheckpoint after failed composite: %v", err)
	}
	if gotCP.AppliedPerService[ref] != "a1b2c3" || gotCP.LastApplied != "a1b2c3" {
		t.Fatalf("failed composite left residue: checkpoint = %+v (want lastApplied/apply @ a1b2c3)", gotCP)
	}

	// --- delivery dedup -------------------------------------------------------
	first, err := st.RecordDelivery(ctx, "gh-delivery-t111-2", now)
	if err != nil || !first {
		t.Fatalf("RecordDelivery first = %v, err=%v; want true/nil", first, err)
	}
	dup, err := st.RecordDelivery(ctx, "gh-delivery-t111-2", now)
	if err != nil || dup {
		t.Fatalf("RecordDelivery second = %v, err=%v; contract: zero rows = duplicate", dup, err)
	}

	// --- serialization lease ---------------------------------------------------
	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-A", time.Second, false); err != nil || !ok {
		t.Fatalf("AcquireLease controller-A = %v, %v", ok, err)
	}
	_, err = st.AcquireLease(ctx, "reconcile", "controller-B", time.Second, false)
	if !errors.Is(err, domain.ErrLocked) {
		t.Fatalf("contested acquire error = %v, want ErrLocked", err)
	}
	var le domain.LockedError
	if !errors.As(err, &le) || le.Holder != "controller-A" {
		t.Fatalf("ErrLocked must carry the holder identity, got %v", err)
	}
	if err := st.ReleaseLease(ctx, "reconcile", "controller-B"); !errors.Is(err, domain.ErrLocked) {
		t.Fatalf("foreign release = %v, want ErrLocked", err)
	}
	if err := st.ReleaseLease(ctx, "reconcile", "controller-A"); err != nil {
		t.Fatalf("own release: %v", err)
	}
	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-B", 50*time.Millisecond, false); err != nil || !ok {
		t.Fatalf("AcquireLease controller-B after release: %v, %v", ok, err)
	}
	// Renew extends for the same holder (idempotent acquire keeps expiry; renew=true pushes it out).
	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-B", 50*time.Millisecond, true); err != nil || !ok {
		t.Fatalf("renew by holder = %v, %v", ok, err)
	}
	// Expiry: after the ttl lapses, a *third* holder takes over the
	// now-expired lease (takeover) — proof the expiry check is real.
	time.Sleep(150 * time.Millisecond)
	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-C", time.Second, false); err != nil || !ok {
		t.Fatalf("takeover after expiry = %v, %v", ok, err)
	}
}

// TestDowngradeProtection seeds an applied goose version above this build's
// knowledge and asserts Open refuses with ErrSchemaNewer carrying the typed
// version detail (state-store.md "Migration expectations"). Uses database/sql
// on the same file with the module's driver — no private sqlite fields.
func TestDowngradeProtection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "downgrade.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer db.Close() //nolint:errcheck // probe cleanup
	if _, err := db.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (?, 1, CURRENT_TIMESTAMP)`,
		sqlite.SchemaVersionMax+10); err != nil {
		t.Fatalf("seed newer schema: %v", err)
	}

	_, err = sqlite.Open(ctx, path)
	if !errors.Is(err, store.ErrSchemaNewer) {
		t.Fatalf("Open on newer schema = %v, want ErrSchemaNewer", err)
	}
	var sne store.SchemaNewerError
	if !errors.As(err, &sne) || sne.Current != sqlite.SchemaVersionMax+10 || sne.Known != sqlite.SchemaVersionMax {
		t.Fatalf("typed error = %v (%+v), want Current %d Known %d",
			err, sne, sqlite.SchemaVersionMax+10, sqlite.SchemaVersionMax)
	}
}
