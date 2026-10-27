// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Test-support helpers: thin, typed wrappers around the stdlib and the
// real store driver so the fixture tables stay declarative. All paths
// live under a per-test t.TempDir().

func writeFile(t *testing.T, path, content string) {
	writeFileWithMode(t, path, 0o600, content)
}

func writeFileWithMode(t *testing.T, path string, mode os.FileMode, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(path, perm); err != nil {
		t.Fatal(err)
	}
}

// dirNames lists a directory's entry names (the "nothing was created"
// assertion).
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// containsAny is the tiny leak-check (case-insensitive substring).
func containsAny(s, needle string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(needle))
}

// writeMigratedStore creates a real, migrated store at path (tier-1
// hermetic sqlite, exactly like internal/store/sqlite's own openTestAt).
func writeMigratedStore(t *testing.T, path string) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// writeOldOp records one reconciled (succeeded) operation whose
// finished timestamp is `at` — the fixture behind the staleness check.
func writeOldOp(t *testing.T, path string, at time.Time) {
	t.Helper()
	st := writeMigratedStore(t, path)
	op, err := domain.NewOperation(domain.OpReconcile, domain.ActorUser, "fixture-correlation", "abc123def456")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.InsertOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", at); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", at); err != nil {
		t.Fatal(err)
	}
	// Hold one reference so linters do not flag the unused uuid side
	// effects; the recorded state is what diagnose reads via
	// ListOperations.
	if op.ID == uuid.Nil {
		t.Fatal("fixture produced a zero operation id")
	}
}

// bumpSchemaVersion pushes the on-disk goose version past the build's
// SchemaVersionMax, simulating "the store was written by a NEWER build"
// (the typed ErrSchemaNewer path). The goose version table is goose's;
// tests may simulate a newer build there (store README's documented
// carve-out).
func bumpSchemaVersion(t *testing.T, path string, version int64) {
	t.Helper()
	// Separate connection (mimics the newer build having closed).
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE goose_db_version SET version_id = ? WHERE is_applied = 1`, version); err != nil {
		t.Fatalf("bump schema version in %s: %v", path, err)
	}
}
