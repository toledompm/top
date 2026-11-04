// SPDX-License-Identifier: MIT

package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backup"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Tier-1 hermetic tests: per-test temp dirs, a fixed fake clock, and a
// real (temp-file) state store as the Registry — the engine is exercised
// over the actual StateStore surface without touching any host layout.
// Everything is caller-invoked by design (scheduler v0 deviation, README).

// fakeRecorder captures the metrics seam.
type fakeRecorder struct {
	successAt  time.Time
	verifyOK   bool
	verifyDone bool
	results    []string
}

func (r *fakeRecorder) SetBackupSuccessAt(t time.Time) { r.successAt = t }
func (r *fakeRecorder) SetBackupVerifyResult(ok bool)  { r.verifyOK, r.verifyDone = ok, true }
func (r *fakeRecorder) RecordBackup(d time.Duration, result string) {
	r.results = append(r.results, result)
}

// testEnv bundles a store + engine over a temp dir with a mutable clock.
type testEnv struct {
	store  *sqlite.Store
	rec    *fakeRecorder
	engine *backup.Engine
	dir    string
	clock  time.Time
}

// newEnv builds an engine against a migrated temp-file store.
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "top.db")
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	impl, ok := st.(*sqlite.Store)
	if !ok {
		t.Fatalf("Open returned %T, want *sqlite.Store", st)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &fakeRecorder{}
	env := &testEnv{
		store: impl,
		rec:   rec,
		dir:   filepath.Join(root, "backups"),
		clock: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
	env.engine = &backup.Engine{
		DBPath:   dbPath,
		Dir:      env.dir,
		Registry: st,
		Recorder: rec,
		Now:      func() time.Time { return env.clock },
	}
	return env
}

// step advances the env clock.
func (env *testEnv) step(d time.Duration) { env.clock = env.clock.Add(d) }

// insertOp records one operation (the watermark source).
func insertOp(t *testing.T, st store.Store) *domain.Operation {
	t.Helper()
	op, err := domain.NewOperation(domain.OpBackup, domain.ActorUser, "", "")
	if err != nil {
		t.Fatalf("NewOperation: %v", err)
	}
	if err := st.InsertOperation(context.Background(), op); err != nil {
		t.Fatalf("InsertOperation: %v", err)
	}
	return op
}

func mustCreate(t *testing.T, e *backup.Engine) *store.Backup {
	t.Helper()
	b, err := e.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return b
}

func backups(t *testing.T, st *sqlite.Store) []*store.Backup {
	t.Helper()
	rows, err := st.ListBackups(context.Background())
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	return rows
}

func TestEngineCreateRoundtrip(t *testing.T) {
	env := newEnv(t)
	insertOp(t, env.store) // watermark source
	last := insertOp(t, env.store)

	b := mustCreate(t, env.engine)
	if _, err := os.Stat(b.Path); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if want := filepath.Join(env.dir, "top-20261103T120000Z.db"); b.Path != want {
		t.Fatalf("filename %q, want %q", b.Path, want)
	}
	if b.Size <= 0 {
		t.Fatalf("size not recorded: %d", b.Size)
	}
	if b.WatermarkOp != last.ID.String() {
		t.Fatalf("watermark %q, want newest op %q", b.WatermarkOp, last.ID)
	}
	if !b.CreatedAt.Equal(env.clock) {
		t.Fatalf("CreatedAt %v, want %v", b.CreatedAt, env.clock)
	}

	rows := backups(t, env.store)
	if len(rows) != 1 || rows[0].Path != b.Path || rows[0].Size != b.Size ||
		rows[0].WatermarkOp != last.ID.String() {
		t.Fatalf("registry readback: %+v vs %+v", rows, b)
	}

	if !env.rec.successAt.Equal(env.clock) {
		t.Fatalf("SetBackupSuccessAt = %v, want %v", env.rec.successAt, env.clock)
	}
	if len(env.rec.results) != 1 || env.rec.results[0] != backup.ResultOK {
		t.Fatalf("RecordBackup = %v, want [ok]", env.rec.results)
	}
}

func TestEngineCreateEmptyHistoryWatermark(t *testing.T) {
	env := newEnv(t) // zero operations exist yet
	b := mustCreate(t, env.engine)
	if b.WatermarkOp != "" {
		t.Fatalf("watermark on empty history = %q, want empty", b.WatermarkOp)
	}
}

func TestEngineCreateSameSecondCollision(t *testing.T) {
	env := newEnv(t)
	first := mustCreate(t, env.engine)
	second, err := env.engine.Create(context.Background())
	if err != nil {
		t.Fatalf("second Create in the same second: %v", err)
	}
	if first.Path == second.Path {
		t.Fatalf("collision: both creates produced %s", first.Path)
	}
	if want := filepath.Join(env.dir, "top-20261103T120000Z-1.db"); second.Path != want {
		t.Fatalf("suffixed name %q, want %q", second.Path, want)
	}
	if _, err := os.Stat(first.Path); err != nil {
		t.Fatalf("first backup overwritten/missing: %v", err)
	}
	if got := len(backups(t, env.store)); got != 2 {
		t.Fatalf("registry rows = %d, want 2", got)
	}
}

func TestEngineCreateMissingLiveDB(t *testing.T) {
	env := newEnv(t)
	env.engine.DBPath = filepath.Join(t.TempDir(), "does-not-exist.db")
	if b, err := env.engine.Create(context.Background()); b != nil || err == nil {
		t.Fatalf("Create on missing live DB = %v, %v; want error", b, err)
	}
	if last := env.rec.results[len(env.rec.results)-1]; last != backup.ResultError {
		t.Fatalf("failure not recorded on metrics: %v", env.rec.results)
	}
	if !env.rec.successAt.IsZero() {
		t.Fatalf("success stamped on failure at %v", env.rec.successAt)
	}
}

func TestEngineRejectsInvalidWiring(t *testing.T) {
	// Plain struct engines must fail loudly, not guess at paths.
	_, err := (&backup.Engine{}).Create(context.Background())
	if !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("Create on zero-value engine err = %v, want ErrInvalid", err)
	}
	for _, e := range []*backup.Engine{
		{DBPath: "ok.db"},                // no Dir
		{DBPath: "ok.db", Dir: "ok-dir"}, // no Registry
	} {
		if _, err := e.Verify(context.Background(), "p"); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("Verify wiring err = %v, want ErrInvalid", err)
		}
	}
}

func TestEngineVerifyGreen(t *testing.T) {
	env := newEnv(t)
	insertOp(t, env.store)
	b := mustCreate(t, env.engine)
	env.step(time.Minute)

	result, err := env.engine.Verify(context.Background(), b.Path)
	if err != nil || result != "ok" {
		t.Fatalf("Verify = %q, %v; want ok", result, err)
	}
	rows := backups(t, env.store)
	if rows[0].VerifyResult != "ok" || rows[0].VerifiedAt.IsZero() {
		t.Fatalf("registry verification stamp: %+v", rows[0])
	}
	if !env.rec.verifyDone || !env.rec.verifyOK {
		t.Fatalf("verify metrics on success = %+v", env.rec)
	}
}

func TestEngineVerifyCorruptFile(t *testing.T) {
	env := newEnv(t)
	b := mustCreate(t, env.engine)
	env.step(time.Minute)

	// Header magic intact, inner pages wrecked: the structural check
	// passes, the integrity check must not.
	raw, err := os.ReadFile(b.Path)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	for i := bytes_range_start; i < bytes_range_end; i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(b.Path, raw, 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	result, err := env.engine.Verify(context.Background(), b.Path)
	if err == nil {
		t.Fatalf("corrupt verify succeeded (%q)", result)
	}
	if !errors.Is(err, domain.ErrCorrupt) {
		t.Fatalf("corruption error class, got: %v", err)
	}
	rows := backups(t, env.store)
	if rows[0].VerifyResult == "ok" || rows[0].VerifyResult == "" {
		t.Fatalf("failure detail not recorded: %+v", rows[0])
	}
	if !env.rec.verifyDone || env.rec.verifyOK {
		t.Fatalf("verify metrics on corruption = %+v", env.rec)
	}
}

// Corruption window inside the first data pages: keeps the 16-byte header
// magic untouched while breaking the page payload.
const (
	bytes_range_start = 100
	bytes_range_end   = 400
)

func TestEngineVerifyUnregisteredPath(t *testing.T) {
	env := newEnv(t)
	name := filepath.Join(env.dir, "rogue.db")
	if err := os.MkdirAll(env.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Content is irrelevant: the registry is consulted FIRST — a file
	// nobody recorded never gets its bytes trusted.
	if err := os.WriteFile(name, []byte("anything"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := env.engine.Verify(context.Background(), name); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unregistered verify err = %v, want ErrNotFound", err)
	}
}

func TestEngineVerifyWatermarkMissingFromCopy(t *testing.T) {
	// The registered row claims the backup contains op "not-there"; the
	// copy's operation history disagrees: the contract's "confirm
	// watermark present" must fail rather than trust the row.
	env := newEnv(t)
	b := mustCreate(t, env.engine)
	stub := &stubRegistry{rows: []*store.Backup{{
		Path:        b.Path,
		WatermarkOp: "not-there",
		CreatedAt:   time.Unix(0, 0),
	}}}
	eng := &backup.Engine{DBPath: env.engine.DBPath, Dir: env.dir, Registry: stub, Now: env.engine.Now}
	if _, err := eng.Verify(context.Background(), b.Path); !errors.Is(err, domain.ErrCorrupt) {
		t.Fatalf("foreign-watermark verify err = %v, want ErrCorrupt", err)
	}
	if len(stub.results) != 1 || stub.results[0] == "ok" {
		t.Fatalf("failure not recorded in registry: %v", stub.results)
	}
}

func TestEngineVerifyHeaderMagicBites(t *testing.T) {
	env := newEnv(t)
	name := filepath.Join(env.dir, "not-sqlite.db")
	if err := os.MkdirAll(env.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("definitely not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := &stubRegistry{rows: []*store.Backup{{Path: name, CreatedAt: time.Unix(0, 0)}}}
	eng := &backup.Engine{DBPath: env.engine.DBPath, Dir: env.dir, Registry: stub, Now: env.engine.Now}
	if _, err := eng.Verify(context.Background(), name); !errors.Is(err, domain.ErrCorrupt) {
		t.Fatalf("text-file verify err = %v, want ErrCorrupt", err)
	}
}

// stubRegistry is the minimal Registry fake (also proves the engine works
// against the seam, not only against the SQLite implementation).
type stubRegistry struct {
	rows    []*store.Backup
	results []string
}

func (*stubRegistry) InsertBackup(context.Context, *store.Backup) error { return nil }
func (s *stubRegistry) ListBackups(context.Context) ([]*store.Backup, error) {
	return s.rows, nil
}

func (s *stubRegistry) SetBackupVerified(_ context.Context, _, result string, _ time.Time) error {
	s.results = append(s.results, result)
	return nil
}

func (*stubRegistry) RemoveBackup(context.Context, string) error {
	return domain.NewNotFound("backup", "stub")
}

func (*stubRegistry) ListOperations(context.Context, int, *store.OperationFilter) ([]*domain.Operation, error) {
	return nil, nil
}

// Compile proof: the shipped SQLite store satisfies the engine's Registry
// seam and the recorder seam is obs-compatible (documented in the README).
var (
	_ backup.Registry = (*sqlite.Store)(nil)
)
