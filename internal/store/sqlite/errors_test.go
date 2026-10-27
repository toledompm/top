package sqlite

import (
	"errors"
	"fmt"
	"testing"

	"github.com/toledompm/top/internal/domain"
)

// errWrap mimics an intermediate wrapper around a raw driver error: a
// plain-fmt-wrapped *sqlite.Error still classifies via errors.As (the
// message-fallback branch is covered by the string cases here).
type errWrap struct{ inner error }

func (e errWrap) Error() string { return fmt.Sprintf("wrapped: %s", e.inner.Error()) }
func (e errWrap) Unwrap() error { return e.inner }

func TestClassifyErrorSemantics(t *testing.T) {
	const busyMsg = "integrity? database file is locked (5) (SQLITE_BUSY)"
	// Persistent SQLITE_BUSY past busy_timeout → ErrLocked with holder
	// identity in the message (state-store contract).
	err := classify("reconcile run", errWrap{fmt.Errorf("%s", busyMsg)}, "reconcile")
	if !errOf(err, domain.ErrLocked) {
		t.Fatalf("classification = %v, want ErrLocked-classified error", err)
	}

	// Corruption codes → ErrCorrupt ("run `topctl recover diagnose`").
	const corruptMsg = "fatal: The database disk image is malformed (SQLITE_CORRUPT)"
	cerr := classify("read", fmt.Errorf("%s", corruptMsg), "")
	if !errOf(cerr, domain.ErrCorrupt) {
		t.Fatalf("corruption = %v, want ErrCorrupt classified", cerr)
	}
	// Open failure of a non-DB file → ErrCorrupt.
	const notADb = "open: file is not a database (26) (SQLITE_NOTADB)"
	if !errOf(classify("open state store (ping x)", fmt.Errorf("%s", notADb), ""), domain.ErrCorrupt) {
		t.Fatal("open failure is not ErrCorrupt classified")
	}

	// Nil passes through untouched.
	if classify("x", nil, "") != nil {
		t.Fatal("nil classified as non-nil")
	}
	// Plain errors keep their class, gain context.
	wrapped := classify("get observed", errors.New("boom"), "")
	if errOf(wrapped, domain.ErrCorrupt) || errOf(wrapped, domain.ErrLocked) || errOf(wrapped, domain.ErrConflict) {
		t.Fatalf("plain error smuggled a wrong class: %v", wrapped)
	}
	if got := wrapped.Error(); got != "get observed: boom" {
		t.Fatalf("context lost: %q", got)
	}
}
