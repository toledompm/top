package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/toledompm/top/internal/exec"
)

// Process exit codes (docs/contracts/cli.md): 0 ok; 1 drift/diagnose
// findings (scripting); 2 usage; 3 operation failed; 4
// backend/store-unavailable. Code 1 is produced by real commands with
// findings: `topctl diff` drift and `topctl doctor` failing checks
// (T-203). Code 4 has no producer yet — it appears when M3 commands
// carry real backend liveness — but the constants exist now so the
// contract has one source of truth.
const (
	ExitOK          = 0
	ExitDrift       = 1
	ExitUsage       = 2
	ExitFailed      = 3
	ExitUnreachable = 4

	// MinDokkuVersion is the oldest Dokku release TOP supports,
	// surfaced by `topctl version` (docs/contracts/cli.md).
	MinDokkuVersion = "0.38"
)

// ErrUsage is the sentinel for invalid input produced by helpers such as
// ParseFormat; callers wrap it in an ExitError{ExitUsage} via render.go.
var ErrUsage = errors.New("usage error")

// notImplementedOp is the shared stub notice. Honest on purpose: it says
// what the command would do and when the capability lands (M2/M3).
const notImplementedOp = "not implemented: planned M2/M3"

// errAs is a thin errors.As alias kept so the nerve center of exit-code
// mapping reads without importing errors twice in each file.
func errAs(err error, target any) bool { return errors.As(err, target) }

// isTTY reports whether r is an interactive character device. Used only
// for the confirm prompt's "ask the human?" branch (ADR-0009); tests
// force the answer via Options.StdinIsTTY so no real TTY is needed.
func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Options parameterizes the command tree for tests and the main binary.
type Options struct {
	// Version is the build stamp (ldflags-injected in cmd/topctl).
	Version string
	// Stdin/Out/Err override the std streams (tests capture with these).
	Stdin io.Reader
	Out   io.Writer
	Err   io.Writer
	// StdinIsTTY forces the confirm prompt's TTY answer (tests). Nil
	// means: detect from Stdin (os.File char-device check).
	StdinIsTTY *bool
	// Runner overrides the process runner for host-touching commands
	// (real bodies from M2 onward: recover; nil → exec.NewFastExec(),
	// i.e. FastExec runs the real host). Tests inject exectest's
	// FakeRunner so tests never touch the real host; the read-only
	// doctor-style commands may, and do, exercise the real machine
	// (documented invariant: tests never run mutating verbs for real).
	Runner exec.Runner
}

// ExitError carries a process exit code through RunE into Execute.
type ExitError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("topctl: exit code %d", e.Code)
	}
	return fmt.Sprintf("topctl: exit code %d: %v", e.Code, e.Err)
}

// Unwrap exposes the underlying error for errors.Is/As.
func (e *ExitError) Unwrap() error { return e.Err }
