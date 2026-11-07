package domain

import (
	"errors"
	"fmt"
)

// Error taxonomy sentinels (contracts/README.md, state-store/platform-backend
// error semantics). Typed errors below all Unwrap() to exactly one sentinel,
// so callers can branch with errors.Is on the class or errors.As on the
// concrete type for detail.
var (
	// ErrNotFound: the referenced entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict: the request conflicts with current reality (entity
	// exists and is not ours, invalid state transition, constraint).
	// Callers decide retry policy.
	ErrConflict = errors.New("conflict")
	// ErrLocked: a persistent serialization-lease/contended-writer failure;
	// the holder identity is in the message. Do not spin-retry blind.
	ErrLocked = errors.New("locked")
	// ErrCorrupt: metadata state is unreadable/corrupt. Never retry —
	// surface the reconstruction hint (state-store contract).
	ErrCorrupt = errors.New("corrupt")
	// ErrInvalid: the input/record fails validation.
	ErrInvalid = errors.New("invalid")
	// ErrUnhealthy: the backend itself is unusable (dokku down/unresponsive).
	ErrUnhealthy = errors.New("backend unhealthy")
	// ErrBackend: the dokku command layer failed (redacted stderr, exit
	// code carried by the concrete error in internal/exec/backend).
	ErrBackend = errors.New("backend error")
)

// hint is the actionable follow-up suffix; every typed error carries one,
// formatted as "… ; hint: ..." per the shared error semantics.
const hintFmt = "%s (hint: %s)"

// NotFoundError is a typed ErrNotFound with the entity kind and reference.
type NotFoundError struct {
	Kind string // e.g. "service", "operation", "plan for service"
	Ref  string
	Hint string
}

// NewNotFound builds a NotFoundError with a standard hint.
func NewNotFound(kind, ref string) error {
	return NotFoundError{Kind: kind, Ref: ref, Hint: "check `topctl history`; the entity may never have been created"}
}

func (e NotFoundError) Error() string {
	return fmt.Sprintf(hintFmt, fmt.Sprintf("%s %q: not found", e.Kind, e.Ref), e.Hint)
}
func (e NotFoundError) Unwrap() error { return ErrNotFound }

// ConflictError is a typed ErrConflict for ownership/existence clashes.
type ConflictError struct {
	Kind   string
	Ref    string
	Reason string
	Hint   string
}

func (e ConflictError) Error() string {
	return fmt.Sprintf(hintFmt, fmt.Sprintf("%s %q: %s", e.Kind, e.Ref, e.Reason), e.hintOrDef("verify ownership with `topctl doctor`"))
}
func (e ConflictError) Unwrap() error { return ErrConflict }

// hintOrDef returns the hint or a default when empty.
func (e ConflictError) hintOrDef(def string) string {
	if e.Hint == "" {
		return def
	}
	return e.Hint
}

// LockedError reports persistent lease/writer contention with holder
// identity in the message (state-store contract).
type LockedError struct {
	Lease  string
	Holder string
	Hint   string
}

func (e LockedError) Error() string {
	return fmt.Sprintf(hintFmt,
		fmt.Sprintf("lease %q is held by %s", e.Lease, e.Holder),
		e.hintOrDef("retry once after the current run finishes; do not loop-retry"))
}
func (e LockedError) Unwrap() error { return ErrLocked }

// hintOrDef returns the hint or a default when empty.
func (e LockedError) hintOrDef(def string) string {
	if e.Hint == "" {
		return def
	}
	return e.Hint
}

// CorruptError reports unreadable/corrupt metadata; per contract it never
// suggests a blind retry — the remedy is rebuilding the store.
type CorruptError struct {
	Detail string
	Hint   string
}

func (e CorruptError) Error() string {
	return fmt.Sprintf(hintFmt, "metadata corrupt: "+e.Detail, e.hintOrDef("the metadata DB is reconstructible: wipe the state file and re-run topctl install; reconcile is idempotent"))
}
func (e CorruptError) Unwrap() error { return ErrCorrupt }

// hintOrDef returns the hint or a default when empty.
func (e CorruptError) hintOrDef(def string) string {
	if e.Hint == "" {
		return def
	}
	return e.Hint
}

// ValidationError is a spec/state validation failure with the offending
// file and field, as non-retryable fail-fast errors require (reconciler
// contract: fail fast, clear error text with file/field).
type ValidationError struct {
	File   string // spec path (or "" when not file-derived)
	Field  string // dotted path within the document
	Reason string
}

func (e ValidationError) Error() string {
	loc := e.Field
	if e.File != "" {
		loc = e.File + ": " + e.Field
	}
	return fmt.Sprintf("%s: invalid: %s", loc, e.Reason)
}
func (e ValidationError) Unwrap() error { return ErrInvalid }

// PlanConflictError reports plan-time conflicts (e.g. two service plans
// contend for the same domain or a dependsOn cycle) so callers can branch
// on the offending service.
type PlanConflictError struct {
	Ref    ServiceRef
	Reason string
	Hint   string
}

func (e PlanConflictError) Error() string {
	return fmt.Sprintf(hintFmt, fmt.Sprintf("service %s: %s", e.Ref, e.Reason),
		e.hintOrDef("inspect the spec set for the offending field"))
}
func (e PlanConflictError) Unwrap() error { return ErrConflict }

func (e PlanConflictError) hintOrDef(def string) string {
	if e.Hint == "" {
		return def
	}
	return e.Hint
}

// InvalidTransitionError is returned in tight loops where the deterministic
// error text matters; Operation.Next embeds the same words inline.
type InvalidTransitionError struct {
	From, To OperationState
}

func (e InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid operation transition %s → %s: %s", e.From, e.To, ErrConflict)
}
func (e InvalidTransitionError) Unwrap() error { return ErrConflict }
