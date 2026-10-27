package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SkillReason explains why a service was skipped in a run (reconciler
// contract: dependents of a failed service are skipped with reason).
type SkipReason string

const (
	// SkipDependencyFailed: a dependsOn prerequisite failed readiness.
	SkipDependencyFailed SkipReason = "dependency-failed"
	// SkipSelfDrift: drift on TOP's own controller is reported, the
	// service is never auto-applied (ADR-0005).
	SkipSelfDrift SkipReason = "self-drift-reported"
	// SkipLocked: another writer holds the reconcile lease.
	SkipLocked SkipReason = "locked"
)

// IsValid reports whether r is a known skip reason.
func (r SkipReason) IsValid() bool {
	switch r {
	case SkipDependencyFailed, SkipSelfDrift, SkipLocked:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (r SkipReason) String() string { return string(r) }

// OutcomeResult is one service's result within a run (reconciler contract
// example: `noop` | `succeeded` | `failed`; plus skipped/orphaned).
type OutcomeResult string

const (
	// OutcomeNoop: plan was empty; nothing applied.
	OutcomeNoop OutcomeResult = "noop"
	// OutcomeSucceeded: changes applied and readiness passed.
	OutcomeSucceeded OutcomeResult = "succeeded"
	// OutcomeFailed: applied-and-failed or apply errored; the old release
	// keeps serving (ADR-0002 failure semantics).
	OutcomeFailed OutcomeResult = "failed"
	// OutcomeSkipped: not attempted this run (SkipReason says why).
	OutcomeSkipped OutcomeResult = "skipped"
	// OutcomeOrphaned: present on host, absent from Git.
	OutcomeOrphaned OutcomeResult = "orphaned"
)

// IsValid reports whether r is a known outcome result.
func (r OutcomeResult) IsValid() bool {
	switch r {
	case OutcomeNoop, OutcomeSucceeded, OutcomeFailed, OutcomeSkipped, OutcomeOrphaned:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (r OutcomeResult) String() string { return string(r) }

// RunStatus is the overall status of a reconciliation run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
)

// IsValid reports whether s is a known run status.
func (s RunStatus) IsValid() bool {
	switch s {
	case RunPending, RunRunning, RunSucceeded, RunFailed:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (s RunStatus) String() string { return string(s) }

// ServiceOutcome is the per-service record of one run (matches the
// reconciler contract's example JSON record).
type ServiceOutcome struct {
	Ref    ServiceRef
	Result OutcomeResult
	// Changes are human-readable change strings from the plan that was
	// applied (e.g. "revision shaA→shaB"); secret references appear as
	// names only.
	Changes []string
	// DurationMs for the service's apply+readiness; 0 when not applied.
	DurationMs int64
	// Err is the redacted error text on failure.
	Err string
	// Hint is the actionable follow-up ("top logs observability/grafana; ...").
	Hint string
	// Skip is set exactly when Result == OutcomeSkipped.
	Skip SkipReason
}

// Validate checks result/skip pairing and that failures explain themselves.
func (o ServiceOutcome) Validate() error {
	if !o.Result.IsValid() {
		return fmt.Errorf("%w: outcome result %q for %v", ErrInvalid, o.Result, o.Ref)
	}
	if o.Result == OutcomeSkipped && !o.Skip.IsValid() {
		return fmt.Errorf("%w: skipped outcome for %v: missing skip reason", ErrInvalid, o.Ref)
	}
	if o.Result == OutcomeFailed && o.Err == "" {
		return fmt.Errorf("%w: failed outcome for %v: empty error text", ErrInvalid, o.Ref)
	}
	return nil
}

// ReconciliationRun is the summary record of one full pipeline execution
// (fetch → validate → per-service apply → checkpoint advance), written to
// the state store and surfaced by `topctl history`.
type ReconciliationRun struct {
	// OperationID links this run to its append-only operation record.
	OperationID uuid.UUID
	// Actor is what triggered the run (webhook, poll→controller, user).
	Actor Actor
	// CorrelationID ties the run to its trigger (e.g. delivery id).
	CorrelationID string
	// Revision the run reconciled toward.
	Revision string
	// Status of the overall run.
	Status RunStatus
	// StartedAt/FinishedAt bound the run; FinishedAt zero until terminal.
	StartedAt  time.Time
	FinishedAt time.Time
	// Outcomes per service (may be a subset when validation failed before
	// any mutation, per the reconciler contract).
	Outcomes []ServiceOutcome
	// ValidationErr is non-empty when the run failed the strict
	// load+validate step before any mutation (per-file errors joined).
	ValidationErr string
}

// Checkpoint is what reconciliation records to resume/rollback from: the
// last revision fully applied, and the last-known-good revision per
// service for rollback (ADR-0002, state-store contract).
type Checkpoint struct {
	// LastApplied is the revision of the last run that got far enough to
	// become the working baseline (advanced even on partial failure for
	// the services that did apply).
	LastApplied string
	// LastKnownGood is the repo-wide revision recorded only when every
	// service in that run came out green ("" when none has yet).
	LastKnownGood string
	// LastKnownGoodPerService maps a service ref to the revision whose
	// release is currently the last known-good for that service (image
	// services carry the source-image reference instead of a repo sha).
	LastKnownGoodPerService map[ServiceRef]RevisionRef
}

// RevisionRef names what last-known-good actually stores: either a Git
// commit sha (source builds) or an image reference (image deploys).
type RevisionRef struct {
	// Kind discriminates the two forms.
	Kind RevisionKind
	// Value is the sha or the image reference.
	Value string
}

// RevisionKind distinguishes source-build (git sha) from image deploys.
type RevisionKind string

const (
	// RevisionGit: a resolved git commit sha of the platform repo.
	RevisionGit RevisionKind = "git"
	// RevisionImage: a docker image reference (digest preferred).
	RevisionImage RevisionKind = "image"
)

// IsValid reports whether k is a known revision kind.
func (k RevisionKind) IsValid() bool {
	switch k {
	case RevisionGit, RevisionImage:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (k RevisionKind) String() string { return string(k) }

// Validate asserts the kind/value pairing: git values are shas, images are
// non-empty refs.
func (r RevisionRef) Validate() error {
	if !r.Kind.IsValid() {
		return fmt.Errorf("%w: revision kind %q", ErrInvalid, r.Kind)
	}
	switch r.Kind {
	case RevisionGit:
		if err := ValidRevision(r.Value); err != nil {
			return fmt.Errorf("revision ref: %w", err)
		}
	case RevisionImage:
		if r.Value == "" {
			return fmt.Errorf("%w: revision ref: empty image reference", ErrInvalid)
		}
	}
	return nil
}
