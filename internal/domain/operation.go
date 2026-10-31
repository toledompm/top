package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OperationKind classifies a tracked mutating action (state-store contract).
type OperationKind string

const (
	OpInstall   OperationKind = "install"
	OpReconcile OperationKind = "reconcile"
	OpDeploy    OperationKind = "deploy"
	OpBackup    OperationKind = "backup"
	OpRestore   OperationKind = "restore"
	OpUninstall OperationKind = "uninstall"
	OpOther     OperationKind = "other" // misc tracked work (e.g. recover diagnose)
)

// IsValid reports whether k is a known operation kind.
func (k OperationKind) IsValid() bool {
	switch k {
	case OpInstall, OpReconcile, OpDeploy, OpBackup, OpRestore, OpUninstall, OpOther:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (k OperationKind) String() string { return string(k) }

// Actor is who initiated an operation (shared semantics, contracts/README).
type Actor string

const (
	// ActorUser: a human through the CLI.
	ActorUser Actor = "user"
	// ActorController: the reconciler acting on its own behalf.
	ActorController Actor = "controller"
	// ActorWebhook: a webhook-triggered reconcile.
	ActorWebhook Actor = "webhook"
	// ActorRecovery: the independent recovery path.
	ActorRecovery Actor = "recovery"
	// ActorSchedule: a scheduled background job (e.g. daily backup).
	ActorSchedule Actor = "schedule"
)

// IsValid reports whether a is a known actor.
func (a Actor) IsValid() bool {
	switch a {
	case ActorUser, ActorController, ActorWebhook, ActorRecovery, ActorSchedule:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (a Actor) String() string { return string(a) }

// OperationState is an operation's lifecycle state:
//
//	pending ──→ running ──→ succeeded
//	   │            │→ failed
//	   └──────────→ aborted      (also running → aborted)
//
// Succeeded/Failed/Aborted are terminal — no state leaves them
// (retries are new operations resuming from recorded checkpoints).
type OperationState string

const (
	// OpPending: recorded, not yet started.
	OpPending OperationState = "pending"
	// OpRunning: work in progress.
	OpRunning OperationState = "running"
	// OpSucceeded: terminal; all effects applied.
	OpSucceeded OperationState = "succeeded"
	// OpFailed: terminal; failed with a redacted error recorded.
	OpFailed OperationState = "failed"
	// OpAborted: terminal; cancelled or superseded before completion.
	OpAborted OperationState = "aborted"
)

// IsValid reports whether s is a known operation state.
func (s OperationState) IsValid() bool {
	switch s {
	case OpPending, OpRunning, OpSucceeded, OpFailed, OpAborted:
		return true
	}
	return false
}

// terminal reports whether s ends an operation's lifecycle.
func (s OperationState) terminal() bool {
	return s == OpSucceeded || s == OpFailed || s == OpAborted
}

// String implements fmt.Stringer.
func (s OperationState) String() string { return string(s) }

// CanTransition reports whether from may move to next per the lifecycle
// above. Terminal states transition to nothing.
func CanTransition(from, next OperationState) bool {
	switch from {
	case OpPending:
		return next == OpRunning || next == OpAborted
	case OpRunning:
		return next == OpSucceeded || next == OpFailed || next == OpAborted
	default:
		return false
	}
}

// Operation is one tracked mutating action: append-only in the state
// store, carrying enough identity (ID, correlationID, revision) to be
// referenced by history, metrics, and checkpoint watermarks.
type Operation struct {
	// ID is the stable, uuidv4 operation identifier (JSON: "op_"-prefixed
	// form is presentation; the stored form is the raw uuid).
	ID uuid.UUID
	// Kind is the operation class.
	Kind OperationKind
	// State is the current lifecycle state.
	State OperationState
	// Actor is who initiated it.
	Actor Actor
	// CorrelationID ties the operation to its cause (e.g. a GitHub
	// delivery id) — never a secret.
	CorrelationID string
	// Revision is the desired-state revision the operation works toward
	// ("" for kinds without one, e.g. backup).
	Revision string
	// StartedAt is set when the operation enters running.
	StartedAt time.Time
	// FinishedAt is set when it enters a terminal state.
	FinishedAt time.Time
	// Err is the redacted error string on failure ("" otherwise). It must
	// never contain a secret value; sub-systems redact before recording.
	Err string
	// Details is a JSON-able structured payload for `topctl history`.
	Details map[string]any
}

// NewOperation builds a pending operation with a fresh uuid. An invalid
// kind/state/actor still returns the operation together with the error so
// callers that construct from user input can reject early.
func NewOperation(kind OperationKind, actor Actor, correlationID, revision string) (*Operation, error) {
	op := &Operation{
		ID:            uuid.New(),
		Kind:          kind,
		State:         OpPending,
		Actor:         actor,
		CorrelationID: correlationID,
		Revision:      revision,
	}
	if !kind.IsValid() {
		return op, fmt.Errorf("%w: operation kind %q", ErrInvalid, kind)
	}
	if !actor.IsValid() {
		return op, fmt.Errorf("%w: actor %q", ErrInvalid, actor)
	}
	return op, nil
}

// Next moves the operation to the next state, enforcing the lifecycle
// transitions and bookkeeping StartedAt/FinishedAt. Invalid transitions
// return a conflict-classified error naming the attempted move.
func (o *Operation) Next(next OperationState) error {
	if !next.IsValid() {
		return fmt.Errorf("operation %s: %w: unknown state %q", o.ID, ErrConflict, next)
	}
	if !CanTransition(o.State, next) {
		return fmt.Errorf("operation %s: invalid transition %s → %s: %w (terminal states and skips are not allowed)",
			o.ID, o.State, next, ErrConflict)
	}
	if next == OpRunning {
		o.StartedAt = time.Now().UTC()
	}
	if next.terminal() {
		o.FinishedAt = time.Now().UTC()
	}
	o.State = next
	return nil
}

// IsTerminal reports whether the operation has reached a terminal state.
func (o *Operation) IsTerminal() bool { return o.State.terminal() }

// Start is Next(OpRunning).
func (o *Operation) Start() error { return o.Next(OpRunning) }

// Succeed moves to succeeded.
func (o *Operation) Succeed() error { return o.Next(OpSucceeded) }

// Fail moves to failed and records the redacted error string. Empty error
// text is rejected — a failure record must explain itself.
func (o *Operation) Fail(redactedErr string) error {
	if redactedErr == "" {
		return fmt.Errorf("operation %s: fail: %w: empty error message", o.ID, ErrInvalid)
	}
	if err := o.Next(OpFailed); err != nil {
		return err
	}
	o.Err = redactedErr
	return nil
}
