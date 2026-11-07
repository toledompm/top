package sqlite

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// openTest opens a migrated store on a temp-file database (never the
// author's machine file layout; testing-strategy tier 1: hermetic).
func openTest(t *testing.T) *Store {
	t.Helper()
	st := openTestAt(t, filepath.Join(t.TempDir(), "top.db"))
	return st
}

func openTestAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, ok := st.(*Store)
	if !ok {
		t.Fatalf("Open returned %T, want *Store", st)
	}
	return s
}

func mustInsertOp(t *testing.T, st *Store, id string, kind domain.OperationKind) *domain.Operation {
	t.Helper()
	op, err := domain.NewOperation(kind, domain.ActorUser, id, "rev-"+id)
	if err != nil {
		t.Fatalf("NewOperation: %v", err)
	}
	if err := st.InsertOperation(context.Background(), op); err != nil {
		t.Fatalf("InsertOperation: %v", err)
	}
	return op
}

func TestMigrateFromZero(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.Check(ctx); err != nil {
		t.Fatalf("Check: %v", err)
	}
	v, err := st.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != SchemaVersionMax {
		t.Fatalf("schema version = %d, want %d", v, SchemaVersionMax)
	}
	// Fresh database yields the zero-valued checkpoint.
	cp, err := st.GetCheckpoint(ctx)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if cp.LastApplied != "" || cp.LastKnownGood != "" || len(cp.AppliedPerService) != 0 {
		t.Fatalf("fresh checkpoint not zero: %+v", cp)
	}
}

func TestMigrationList(t *testing.T) {
	// The embedded list must match the schema: every migration file present
	// is sequential, and the max remains SchemaVersionMax.
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
	if got := entries[len(entries)-1].Name(); got != "0001_init.sql" {
		t.Fatalf("newest migration = %s, want 0001_init.sql (bump SchemaVersionMax with each new file)", got)
	}
}

func TestOperationLifecycle(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	op := mustInsertOp(t, st, "corr-1", domain.OpReconcile)

	// Duplicate insert is a conflict.
	dup, _ := domain.NewOperation(domain.OpDeploy, domain.ActorUser, "", "")
	dup.ID = op.ID // same UUID, different kind
	if err := st.InsertOperation(ctx, dup); errOf(err, domain.ErrConflict) {
		// ok
	} else {
		t.Fatalf("duplicate insert err = %v, want ErrConflict", err)
	}

	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", time.Time{}); err != nil {
		t.Fatalf("→ running: %v", err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", time.Time{}); err != nil {
		t.Fatalf("→ succeeded: %v", err)
	}

	// Terminal states reject further transitions (contract state machine).
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", time.Time{}); errOf(err, domain.ErrConflict) {
	} else {
		t.Fatalf("succeeded → running err = %v, want ErrConflict", err)
	}
	// Skips are rejected too.
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpFailed, "x", time.Time{}); errOf(err, domain.ErrConflict) {
	} else {
		t.Fatalf("succeeded → failed err = %v, want ErrConflict", err)
	}

	// Round-trip with filter and details JSON.
	ops, err := st.ListOperations(ctx, 10, &store.OperationFilter{
		Kind: domain.OpReconcile, State: domain.OpSucceeded, CorrelationID: "corr-1",
	})
	if err != nil {
		t.Fatalf("ListOperations: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("ListOperations = %d ops, want 1", len(ops))
	}
	got := ops[0]
	if got.ID != op.ID || got.State != domain.OpSucceeded {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.StartedAt.IsZero() || got.FinishedAt.IsZero() {
		t.Fatalf("started/finished not stamped: %+v", got)
	}

	// Failure requires a redacted error text.
	op2 := mustInsertOp(t, st, "corr-2", domain.OpDeploy)
	if err := st.UpdateOperationState(ctx, op2.ID, domain.OpRunning, "", time.Time{}); err != nil {
		t.Fatalf("→ running: %v", err)
	}
	if err := st.UpdateOperationState(ctx, op2.ID, domain.OpFailed, "", time.Time{}); errOf(err, domain.ErrInvalid) {
	} else {
		t.Fatalf("fail with empty error err = %v, want ErrInvalid", err)
	}
	if err := st.UpdateOperationState(ctx, op2.ID, domain.OpFailed, "backend: <redacted>", time.Time{}); err != nil {
		t.Fatalf("→ failed: %v", err)
	}
	ops, _ = st.ListOperations(ctx, 1, &store.OperationFilter{State: domain.OpFailed})
	if len(ops) != 1 || ops[0].Err != "backend: <redacted>" {
		t.Fatalf("failed round-trip: %+v", ops)
	}

	// Unknown operation is ErrNotFound.
	if err := st.UpdateOperationState(ctx, missingUUID(), domain.OpRunning, "", time.Time{}); errOf(err, domain.ErrNotFound) {
	} else {
		t.Fatalf("missing op err = %v, want ErrNotFound", err)
	}
}

func TestObservedStateRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	ref := domain.Ref("default", "grafana")
	obs := &domain.ObservedState{
		Ref: ref, AppName: "default-grafana", Deployed: true, Running: true,
		Revision: "abc123", LastBuild: domain.BuildSucceeded,
	}
	now := time.Now().UTC().Round(time.Microsecond)
	if err := st.UpsertObserved(ctx, ref, obs, "abc123", now); err != nil {
		t.Fatalf("UpsertObserved: %v", err)
	}
	got, revision, at, err := st.GetObserved(ctx, ref)
	if err != nil {
		t.Fatalf("GetObserved: %v", err)
	}
	if revision != "abc123" || !at.Equal(now) || !got.Deployed || got.Revision != "abc123" {
		t.Fatalf("round-trip mismatch: %+v rev=%s at=%v", got, revision, at)
	}
	if _, _, _, err := st.GetObserved(ctx, domain.Ref("default", "nope")); errOf(err, domain.ErrNotFound) {
	} else {
		t.Fatalf("missing observed err = %v, want ErrNotFound", err)
	}
}

func TestObservedUpsertReplaces(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	ref := domain.Ref("core", "controller")
	if err := st.UpsertObserved(ctx, ref, &domain.ObservedState{Ref: ref}, "r1", time.Now()); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := st.UpsertObserved(ctx, ref, &domain.ObservedState{Ref: ref, Deployed: true}, "r2", time.Now()); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, rev, _, err := st.GetObserved(ctx, ref)
	if err != nil {
		t.Fatalf("GetObserved: %v", err)
	}
	if rev != "r2" || !got.Deployed {
		t.Fatalf("upsert did not replace: rev=%s %+v", rev, got)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	ref := domain.Ref("observability", "metrics")
	want := &store.Checkpoint{
		LastApplied:   "d5e6f70",
		LastKnownGood: "a1b2c3d",
		LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{
			ref: {Kind: domain.RevisionGit, Value: "a1b2c3d"},
		},
		AppliedPerService: map[domain.ServiceRef]string{ref: "d5e6f70"},
	}
	if err := st.SetCheckpoint(ctx, want); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	got, err := st.GetCheckpoint(ctx)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if got.LastApplied != "d5e6f70" || got.LastKnownGood != "a1b2c3d" ||
		got.LastKnownGoodPerService[ref].Value != "a1b2c3d" || got.AppliedPerService[ref] != "d5e6f70" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRecordDeliveryDedup(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	first, err := st.RecordDelivery(ctx, "d-1", time.Time{})
	if err != nil || !first {
		t.Fatalf("first record = %v, %v; want (true, nil)", first, err)
	}
	dup, err := st.RecordDelivery(ctx, "d-1", time.Now())
	if err != nil || dup {
		t.Fatalf("second record = %v, %v; want (false, nil)", dup, err)
	}
	if _, err := st.RecordDelivery(ctx, "", time.Time{}); errOf(err, domain.ErrInvalid) {
	} else {
		t.Fatalf("empty id err = %v, want ErrInvalid", err)
	}
}

func TestDowngradeProtection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "top.db")
	st := openTestAt(t, path)
	seedNewerSchemaVersion(t, st, SchemaVersionMax+10)
	_ = st.Close()

	_, err := Open(context.Background(), path)
	if err == nil {
		t.Fatal("Open on newer schema = nil error, want ErrSchemaNewer")
	}
	if errOf(err, store.ErrSchemaNewer) {
	} else {
		t.Fatalf("err = %v, want ErrSchemaNewer", err)
	}
	// The typed version detail is present.
	want := store.SchemaNewerError{Current: SchemaVersionMax + 10, Known: SchemaVersionMax}
	if got, ok := asType[store.SchemaNewerError](err); ok && got.Current == want.Current && got.Known == want.Known {
	} else {
		t.Fatalf("typed error = %v, want {Current:%d Known:%d}", err, want.Current, want.Known)
	}
}

// --- small helpers ---------------------------------------------------------

// errOf reports whether err falls in the s error class.
func errOf(err error, s error) bool {
	return errors.Is(err, s)
}

// asType extracts a typed error value via errors.As.
func asType[Z error](err error) (Z, bool) {
	var z Z
	ok := errors.As(err, &z)
	return z, ok
}

// missingUUID returns a uuid that cannot exist in a fresh store.
func missingUUID() uuid.UUID { return uuid.New() }

// seedNewerSchemaVersion inserts an applied goose version above max — used
// to simulate a database migrated by a newer build (downgrade protection).
func seedNewerSchemaVersion(t *testing.T, st *Store, version int64) {
	t.Helper()
	_, err := st.db.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp) VALUES (?, 1, CURRENT_TIMESTAMP)`,
		version)
	if err != nil {
		t.Fatalf("seed goose version %d: %v", version, err)
	}
}
