// SPDX-License-Identifier: MIT

package webhook

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// newTestStore opens a real, migrated temp-file SQLite state store and
// hands it to the handler as the Deduper (testing-strategy tier 1: hermetic
// temp dir, never a real host path). This exercises the actual
// deliveries(deliveryID PK, received_at) table and its INSERT OR IGNORE
// dedup semantics — no fake in the critical path.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "top.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestRealStoreDedupRoundTrip pins the store contract this package relies
// on: identical delivery ids report first=true exactly once, ever after a
// reopen of the same database file (durability), which is what makes a
// controller restart not double-enqueue a still-live delivery.
func TestRealStoreDedupRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "top.db")

	st1, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	at := time.Now()
	first, err := st1.RecordDelivery(context.Background(), "delivery-42", at)
	if err != nil || !first {
		t.Fatalf("first RecordDelivery = (%t, %v), want true, nil", first, err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()
	dup, err := st2.RecordDelivery(context.Background(), "delivery-42", at)
	if err != nil || dup {
		t.Fatalf("reopened RecordDelivery = (%t, %v), want false, nil", dup, err)
	}
	// A different id is still first-seen.
	other, err := st2.RecordDelivery(context.Background(), "delivery-43", at)
	if err != nil || !other {
		t.Fatalf("different id = (%t, %v), want true, nil", other, err)
	}
}
