// SPDX-License-Identifier: MIT

// Typed errors for the Dokku adapter. Per the platform-backend contract: a
// non-zero dokku exit is domain.ErrBackend carrying the redacted stderr and
// exit code; the backend itself being unusable (dokku missing, unresponsive,
// timing out) is domain.ErrUnhealthy. The concrete *BackendError also keeps
// the *exec.ExitError (or other cause) reachable for errors.As/Is callers
// and never re-renders stderr itself — the exec layer has already redacted
// it (ADR-0008: redaction is the exec layer's job; the adapter only ever
// passes pre-redacted buffers through).
package dokku

import (
	"errors"
	"fmt"
	"strings"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
)

// BackendError is the adapter's typed error for every operation failure.
// It unwraps to BOTH its cause (typically *exec.ExitError with the exit
// code and redacted stderr, or exec.ErrTimeout / exec.ErrNotFound for the
// process-level failures) and a class sentinel — ErrBackend for operation
// failures, ErrUnhealthy for "the backend itself is broken" (process
// timeouts, missing binary, backend-health checks) — so callers can branch
// on the class with errors.Is and on detail with errors.As to
// *exec.ExitError.
type BackendError struct {
	// Op is a stable lowercase operation label ("ensure app", "inspect",
	// "stop", "destroy", "health", "ensure network", "last revision").
	Op string
	// App is the target (Dokku app name, network name, or other argv head).
	App string
	// Err is the cause.
	Err error
	// Unhealthy marks errors where the backend itself is unusable (health
	// checks, timeouts, missing binaries) rather than a failed dokku
	// operation; it selects the ErrUnhealthy class sentinel.
	Unhealthy bool
}

// Unwrap exposes both the cause and the class sentinel. A health/timeout/
// missing-binary cause maps to domain.ErrUnhealthy; every other cause maps
// to domain.ErrBackend (the operation ran and Dokku answered).
func (e *BackendError) Unwrap() []error {
	class := domain.ErrBackend
	if e.Unhealthy || errors.Is(e.Err, exec.ErrTimeout) || errors.Is(e.Err, exec.ErrNotFound) {
		class = domain.ErrUnhealthy
	}
	return []error{e.Err, class}
}

// Error renders the canonical one-liner. The cause already carries the
// redacted stderr excerpt (exec.ExitError renders it), so this only adds
// the operation/target context in the shape "dokku <argv>: exit N" plus op.
func (e *BackendError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "backend %s %q: %v", e.Op, e.App, e.Err)
	return b.String()
}

// wrapBackend folds a fatal runner/dokku error into a *BackendError. nil
// stays nil.
func wrapBackend(op, app string, err error) error {
	if err == nil {
		return nil
	}
	return &BackendError{Op: op, App: app, Err: err}
}

// stdoutExitError decorates a failed dokku command with a redacted stdout
// excerpt. Under sudo/non-TTY, dokku writes its failure text to STDOUT
// (the same stream choice that puts the no-changes marker on stdout —
// DeployImage's classifier keys on both), so a deploy failure carrying
// only the stderr excerpt renders as a bare "exit 1" with no reason
// (media-server 2026-11-14: a manifest-unknown git:from-image surfaced
// as exactly that). The stdout buffer is already redacted by the exec
// layer before return (ADR-0008); the adapter only trims it.
type stdoutExitError struct {
	err    error
	stdout string
}

func (e *stdoutExitError) Error() string {
	if t := strings.TrimSpace(e.stdout); t != "" {
		if len(t) > exec.ExcerptLimit {
			t = t[:exec.ExcerptLimit] + "…"
		}
		return fmt.Sprintf("%v (stdout: %s)", e.err, t)
	}
	return e.err.Error()
}

func (e *stdoutExitError) Unwrap() error { return e.err }

// wrapDeployFailure folds a failed deploy command into a BackendError
// carrying the command's stdout excerpt (see stdoutExitError).
func wrapDeployFailure(err error, op, app, stdout string) error {
	if err == nil {
		return nil
	}
	return wrapBackend(op, app, &stdoutExitError{err: err, stdout: stdout})
}

// wrapHealth folds a backend-health failure into a *BackendError whose
// class is ErrUnhealthy (the backend itself, not one operation, is broken).
func wrapHealth(op, app string, err error) error {
	if err == nil {
		return nil
	}
	return &BackendError{Op: op, App: app, Err: err, Unhealthy: true}
}
