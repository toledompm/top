//go:build conformance

package conformance_check

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/exec"
)

// RunFastExec keeps the probe terse (FastExec is the production Runner).
func runFastExec(t *testing.T, f *exec.FastExec, name string, args ...string) ([]byte, []byte, error) {
	t.Helper()
	return f.Run(context.Background(), name, args...)
}

// TestExecExitErrorProbe pins platform-backend.md's error semantics at the
// exec layer: dokku non-zero exit is a typed *exec.ExitError carrying the
// exit code and (redacted) stderr; exit-code matching goes through
// exec.IsExitCode; a missing binary is ErrNotFound; the redaction invariant
// holds through a real child process (helper-echo, hermetically built in
// TestMain — same pattern as internal/exec's tests).
func TestExecExitErrorProbe(t *testing.T) {
	const secretVal = "super-secret-value-77"
	r := &exec.FastExec{RedactSeed: []string{secretVal}}

	// helper-echo with no flags: exit 0, argument round-trip.
	stdout, _, err := runFastExec(t, r, helperEcho, "hello", "top")
	if err != nil || !strings.Contains(string(stdout), "hello top") {
		t.Fatalf("Run helper-echo: out=%q err=%v", stdout, err)
	}

	// Non-zero exit → *exec.ExitError with code 1; stderr flowed through
	// the child and must come back redacted.
	_, stderr, err := runFastExec(t, r, helperEcho, "-fail", secretVal)
	if !exec.IsExitCode(err, 1) || exec.IsExitCode(err, 2) {
		t.Fatalf("err=%v, want IsExitCode(1)==true and IsExitCode(2)==false", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode != 1 {
		t.Fatalf("errors.As *exec.ExitError: err=%v", err)
	}
	if strings.Contains(string(stderr), secretVal) {
		t.Fatalf("stderr leaked secret: %q", stderr)
	}
	if !strings.Contains(string(stderr), "***") {
		t.Fatalf("stderr not redacted: %q", stderr)
	}
	if !strings.Contains(ee.Error(), "exit 1") {
		t.Fatalf("ExitError.Error() = %q, want canonical one-liner w/ 'exit 1'", ee.Error())
	}

	// Missing binary → exec.ErrNotFound.
	_, _, err = runFastExec(t, r, "definitely-not-topctl-xyz-999")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("missing binary err = %v, want exec.ErrNotFound", err)
	}
}
