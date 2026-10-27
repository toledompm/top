package exec

import (
	"errors"
	"fmt"
	"strings"
)

// ExitError reports a non-zero exit status. It carries the structured facts
// (name/argv/exit code/stderr) so callers can branch on code; Error() renders
// a redacted, human-oriented one-liner in the shape mandated by ADR-0009:
//
//	dokku apps:create my-app: exit 1: <redacted stderr excerpt>
//
// Stderr is already redacted by the time it is stored here (FastExec
// registers RedactSeed before returning), so Error() needs no re-check — the
// constructors promise it.
type ExitError struct {
	// Err is the wrapped *os/exec.ExitError, preserved for Unwrap
	// compatibility (errors.As to it still works).
	Err      error
	Name     string
	Args     []string
	ExitCode int

	// Stderr is the redacted stderr buffer (nil if the child wrote nothing).
	Stderr []byte
}

// Error renders the canonical redacted one-liner.
func (e *ExitError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: exit %d", e.Name, strings.Join(e.Args, " "), e.ExitCode)
	if len(e.Stderr) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, ": %s", Excerpt(e.Stderr))
	return b.String()
}

// Unwrap exposes the underlying os/exec.ExitError.
func (e *ExitError) Unwrap() error { return e.Err }

// Excerpt renders the first line of stderr, truncated to ExcerptLimit bytes,
// for embedding in error text. Input is expected to be pre-redacted.
func Excerpt(stderr []byte) string {
	s := string(stderr)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > ExcerptLimit {
		s = s[:ExcerptLimit] + "…"
	}
	if s == "" {
		s = "(no stderr)"
	}
	return s
}

// IsExitCode reports whether err is (or wraps) an *ExitError with the given
// exit status.
func IsExitCode(err error, code int) bool {
	var e *ExitError
	if !errors.As(err, &e) {
		return false
	}
	return e.ExitCode == code
}
