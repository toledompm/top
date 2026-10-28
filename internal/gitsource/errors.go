// SPDX-License-Identifier: MIT

package gitsource

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/toledompm/top/internal/exec"
)

// Typed error space per docs/contracts/git-source.md. Every failure this
// package reports carries a Kind so callers (reconciler, controller loop)
// can decide retry/mutate/halt without parsing stderr; the sentinels are
// the same names the contract text uses.
var (
	// ErrGitAuth: deploy key not granted, key wrong, or host-key
	// verification failed. NEVER auto-bypassed (contract + ADR-0008).
	ErrGitAuth = errors.New("gitsource: git auth failure")
	// ErrGitUnavailable: remote unreachable (network down, DNS, refused).
	// Retried with backoff per ADR-0003.
	ErrGitUnavailable = errors.New("gitsource: git remote unreachable")
	// ErrRevisionGone: the requested revision (or branch) no longer exists
	// on the remote — a force-push rewrote the source of truth. The run
	// fails before any mutation; a human should look.
	ErrRevisionGone = errors.New("gitsource: revision vanished from remote")
	// ErrHostKey is the host-key-verification member of the auth family,
	// kept distinct so remediation can say "pinned known_hosts mismatch"
	// instead of "key not granted".
	ErrHostKey = errors.New("gitsource: host key verification failed")
	// ErrInvalidSHA protects argv: FetchRevision only accepts hex shas.
	ErrInvalidSHA = errors.New("gitsource: invalid revision sha")
	// ErrGitInternal: a git failure with NO recognized signature. Loud but
	// opaque — better than guessing a retry class.
	ErrGitInternal = errors.New("gitsource: unrecognized git failure")

	// Scope aliases (task vocabulary ↔ contract vocabulary).
	ErrAuthFailed   = ErrGitAuth
	ErrUnreachable  = ErrGitUnavailable
	ErrNotFound     = ErrRevisionGone
	ErrUnrecognized = ErrGitInternal
)

// Kind classifies a gitsource.Error for switch-style handling.
type Kind int

const (
	KindUnknown      Kind = iota
	KindAuth              // wraps ErrGitAuth
	KindHostKey           // wraps ErrHostKey (inside auth family)
	KindUnreachable       // wraps ErrGitUnavailable
	KindRevisionGone      // wraps ErrRevisionGone
	KindInternal          // wraps ErrGitInternal
)

func (k Kind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindHostKey:
		return "host-key"
	case KindUnreachable:
		return "unreachable"
	case KindRevisionGone:
		return "revision-gone"
	case KindInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Error is gitsource's typed failure: the op that failed, its kind, the
// (already exec-redacted) stderr excerpt, and a remediation hint. The
// chain to the underlying exec error is preserved for errors.Is/As.
type Error struct {
	Kind   Kind
	Op     string
	Err    error  // underlying error (exec.ExitError, wrapped sentinel, …)
	Stderr string // first stderr line, excerpted; "" when none
	Hint   string // remediation hint; "" when none
}

func (e *Error) Error() string {
	parts := []string{"gitsource: ", e.Op, ": ", e.Kind.String()}
	if e.Err != nil {
		parts = append(parts, ": ", e.Err.Error())
	}
	if e.Stderr != "" {
		parts = append(parts, fmt.Sprintf(" (remote said: %s)", e.Stderr))
	}
	if e.Hint != "" {
		parts = append(parts, "; ", e.Hint)
	}
	return strings.Join(parts, "")
}

func (e *Error) Unwrap() error { return e.Err }

// wrapOp folds any error from op into the typed space. Non-git errors
// (context cancellation, binary missing, programming errors) pass through
// their kind mapping too, so callers can always errors.As a *Error from a
// gitsource call.
func wrapOp(op string, err error) error { return toTyped(op, err, "") }

// firstLine renders the first stderr line, capped to exec.ExcerptLimit.
func firstLine(stderr []byte) string {
	if len(stderr) == 0 {
		return ""
	}
	s := exec.Excerpt(stderr) // first line, truncated, pre-redacted upstream
	return s
}

// toTyped is the single classification funnel.
func toTyped(op string, err error, remoteSaid string) error {
	if err == nil {
		return nil
	}
	var ge *Error
	if errors.As(err, &ge) { // already typed; keep identity
		return err
	}
	if errors.Is(err, ErrGitAuth) {
		return &Error{Kind: KindAuth, Op: op, Err: err, Stderr: remoteSaid}
	}
	if errors.Is(err, ErrHostKey) {
		return &Error{Kind: KindHostKey, Op: op, Err: err, Stderr: remoteSaid}
	}
	if errors.Is(err, ErrRevisionGone) {
		return &Error{Kind: KindRevisionGone, Op: op, Err: err, Stderr: remoteSaid}
	}
	if errors.Is(err, ErrInvalidSHA) {
		return &Error{Kind: KindInternal, Op: op, Err: err}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrTimeout) {
		return &Error{
			Kind: KindInternal, Op: op, Err: err, Stderr: remoteSaid,
			Hint: "cancelled or timed out; nothing to remediate",
		}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &Error{
			Kind: KindInternal, Op: op, Err: err,
			Hint: "git (or tar) binary missing on the controller host",
		}
	}

	// git exits 128 for essentially every transport-level failure; the
	// stderr signature decides the kind.
	kind, hint := classifyStderr(remoteSaid)
	sentinel := sentinelFor(kind)
	return &Error{Kind: kind, Op: op, Err: fmt.Errorf("%w(%v)", sentinel, err), Stderr: remoteSaid, Hint: hint}
}

// signature tables: remote stderr → kind + remediation hint.
func classifyStderr(said string) (Kind, string) {
	if said == "" {
		return KindInternal, "no stderr signature; run with debug logging"
	}
	low := strings.ToLower(said)
	switch {
	case strings.Contains(low, "host key verification failed"),
		strings.Contains(low, "remote host identification has changed"),
		strings.Contains(low, "no matching host key type found"),
		strings.Contains(low, "no matching macs"),
		strings.Contains(low, "no matching cipher"):
		return KindHostKey, "pinned known_hosts does not match the remote host; NEVER disable verification — re-pin the host's key out-of-band, or investigate a possible machine swap"
	case strings.Contains(low, "permission denied"),
		strings.Contains(low, "authentication failed"),
		strings.Contains(low, "publickey"),
		strings.Contains(low, "repository not found"):
		return KindAuth, "deploy key not granted to this repo (or wrong key file); the key stays read-only per ADR-0008"
	case strings.Contains(low, "couldn't find remote ref"),
		strings.Contains(low, "can't find remote ref"),
		strings.Contains(low, "not found in upstream origin"),
		strings.Contains(low, "expected initial ref"):
		return KindRevisionGone, "source of truth was rewritten (force-push); resolve the branch again and fetch the new sha — do NOT delete or overwrite local state, a human should look"
	case strings.Contains(low, "could not resolve host"),
		strings.Contains(low, "getaddrinfo"),
		strings.Contains(low, "connection refused"),
		strings.Contains(low, "connection timed out"),
		strings.Contains(low, "operation timed out"),
		strings.Contains(low, "connect to host"),
		strings.Contains(low, "network is unreachable"),
		strings.Contains(low, "no route to host"):
		return KindUnreachable, "remote or network down; retried with backoff per ADR-0003"
	default:
		return KindInternal, "unrecognized remote failure; check controller logs"
	}
}

// sentinelFor gives each classified kind its contract-named sentinel so
// callers can errors.Is without the *Error.
func sentinelFor(k Kind) error {
	switch k {
	case KindAuth:
		return ErrGitAuth
	case KindHostKey:
		return ErrHostKey
	case KindUnreachable:
		return ErrGitUnavailable
	case KindRevisionGone:
		return ErrRevisionGone
	default:
		return ErrGitInternal
	}
}
