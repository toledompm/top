package sqlite

import (
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/toledompm/top/internal/domain"
)

// codeClassMask strips extended-result bits so (code & mask) compares to
// the primary SQLite result code.
const codeClassMask = 0xff

// containsFold is a case-insensitive substring match for driver messages.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// classify maps a driver error into the state-store contract's typed error
// semantics, wrapped (fmt.Errorf ": %w") so errors.Is/As branch at the call
// site:
//
//   - SQLITE_BUSY / SQLITE_LOCKED after busy_timeout → ErrLocked with
//     holder identity in the message (what names the contention point,
//     e.g. the lease; otherwise the generic write-lock holder)
//   - SQLITE_CORRUPT / SQLITE_NOTADB → ErrCorrupt: never retried; surface
//     "run `topctl recover diagnose`"
//   - SQLITE_CONSTRAINT → ErrConflict for the known unique-key sites
//     (what names the clashing record)
//   - everything else: wrapped with context, class unchanged.
//
// Classification keys on the driver's *sqlite.Error result code; when the
// error is not directly that type (intermediate wrapping), the message is
// matched as a fallback so classes survive wrapping. err == nil → nil.
func classify(op string, err error, what string) error {
	if err == nil {
		return nil
	}
	classErr, _ := upstreamClass(err, what)
	if classErr != nil {
		return fmt.Errorf("%s: %w", op, classErr)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// upstreamClass decides the error class from a driver error's result code
// (or a wrapped message mentioning it). It returns nil for "no contract
// class", leaving the original error untouched.
func upstreamClass(err error, what string) (error, bool) {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		code := serr.Code() & codeClassMask
		switch {
		case code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED: //nolint:staticcheck // QF1002: if/else ladder mirrors the driver's own table
			return domain.LockedError{
				Lease:  firstNonEmpty(what, "writer"),
				Holder: "unknown writer (SQLITE_BUSY persisted past busy_timeout)",
			}, true
		case code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB ||
			code == sqlite3.SQLITE_CANTOPEN:
			return domain.CorruptError{Detail: serr.Error()}, true
		case code == sqlite3.SQLITE_CONSTRAINT:
			return domain.ConflictError{
				Kind:   "constraint",
				Ref:    what,
				Reason: firstNonEmpty(serr.Error(), "constraint violation"),
				Hint:   "the record already exists or references a missing row; decide retry policy at the call site",
			}, true
		}
		return nil, false
	}
	// Fallback for wrapped driver messages (classification survives
	// intermediate fmt.Errorf layers).
	msg := err.Error()
	switch {
	case containsFold(msg, "SQLITE_BUSY") || containsFold(msg, "database is locked") ||
		containsFold(msg, "SQLITE_LOCKED"):
		return domain.LockedError{
			Lease:  firstNonEmpty(what, "writer"),
			Holder: "unknown writer (SQLITE_BUSY persisted past busy_timeout)",
		}, true
	case containsFold(msg, "SQLITE_CORRUPT") || containsFold(msg, "SQLITE_NOTADB") ||
		containsFold(msg, "database disk image is malformed"):
		return domain.CorruptError{Detail: msg}, true
	}
	return nil, false
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
