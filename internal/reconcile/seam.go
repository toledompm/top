// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// StateStore is the reconciler's persistence seam. v0 it is exactly the
// store.Store contract surface (one SQLite implementation), aliased here so
// the reconciler names its dependency, not the store's identity; M4+ may
// narrow it without caller-visible surgery.
type StateStore = store.Store

// Backend is the reconciler's runtime seam (docs/contracts/platform-backend.md).
// It is the structural-typing contract internal/reconcile depends on: any
// backend implementation (today internal/backend/dokku) satisfies it by
// having the methods, by name, in this exact shape.
//
// Existing adapter surface (T-201/T-202/T-205..T-208): EnsureApp, Inspect,
// LastGoodRevision, Stop, Destroy, DeployImage, EnsureNetwork,
// EnsureStorageEntry, AttachStorage, DetachStorage.
//
// Convergence additions required of the same-wave adapter (T-305/T-306
// apply step) — each maps one Dokku command class, batched to avoid
// redeploys (README "Convergence method contract"):
//
//   - SetConfigEnv   → `config:set --no-restart` (additive, values are plain)
//   - SetSecretEnv   → same, carrying a RESOLVED value (never logged)
//   - SetPorts       → `ports:set` (full replace)
//   - SetDomains     → `domains:set` (full replace)
//   - SetRestartPolicy → `ps:set restart-policy`
//   - AttachNetwork  → `network:set` attach (no redeploy)
//
// All methods respect ctx cancellation and return the platform-backend
// typed error classes.
type Backend interface {
	EnsureApp(ctx context.Context, ref domain.ServiceRef) error
	Inspect(ctx context.Context, ref domain.ServiceRef) (domain.ObservedState, error)
	LastGoodRevision(ctx context.Context, ref domain.ServiceRef) (string, error)
	Stop(ctx context.Context, ref domain.ServiceRef) error
	Destroy(ctx context.Context, ref domain.ServiceRef) error
	DeployImage(ctx context.Context, ref domain.ServiceRef, imageRef string) error
	EnsureNetwork(ctx context.Context, name string) error
	EnsureStorageEntry(ctx context.Context, name, hostDir string) error
	AttachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error
	DetachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error

	// Convergence surface (new; lands in internal/dokku same wave).
	SetConfigEnv(ctx context.Context, ref domain.ServiceRef, env map[string]string) error
	SetSecretEnv(ctx context.Context, ref domain.ServiceRef, name, value string) error
	SetPorts(ctx context.Context, ref domain.ServiceRef, ports []domain.Port) error
	SetDomains(ctx context.Context, ref domain.ServiceRef, domains []string) error
	SetRestartPolicy(ctx context.Context, ref domain.ServiceRef, policy domain.RestartPolicy) error
	AttachNetwork(ctx context.Context, ref domain.ServiceRef, network string) error
}

// Clock is the injected time source for pacing and deadlines: readiness
// polls and run duration measurements use only this. The default is
// WallClock; tests use a controlled fake (predicted Now, insta-advanced
// Wait) so gating is exercised without sleeping.
type Clock interface {
	// Now is the current logical/wall time (UTC-relevant fields only are
	// compared).
	Now() time.Time
	// Wait sleeps for d, returning early with ctx.Err() when ctx is done.
	Wait(ctx context.Context, d time.Duration) error
}

// WallClock is the production Clock.
type WallClock struct{}

// Now returns the current time.
func (WallClock) Now() time.Time { return time.Now().UTC() }

// Wait sleeps d or returns early on ctx cancellation.
func (WallClock) Wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// SecretResolver resolves secret references to their values at apply time
// (ADR-0008: names travel through specs and plans; only the apply step
// turns a name into a value). Resolved values are passed straight to
// Backend.SetSecretEnv and never logged or recorded.
type SecretResolver interface {
	// Resolve returns the secret value for name and its version (e.g. a
	// content checksum). An unresolvable reference is an error.
	Resolve(ctx context.Context, name string) (value, version string, err error)
}

// PruneGate is the two-key delete rule of ADR-0009 (ADR-0009 layer 3):
// a prune deletion executes only when the gate allows. The default
// implementation (LedgerPruneGate) delegates to inventory.Deletable, so
// prune NEVER deletes directly — it produces decisions, and only a fresh
// inventory snapshot plus an owned ledger row turn one into a Destroy.
type PruneGate interface {
	// CanDelete applies the two-key rule to the service's derived app.
	// allowed is true with empty reason only when both keys agree.
	CanDelete(ctx context.Context, ref domain.ServiceRef) (allowed bool, reason string, err error)
}

// Failure hints per the reconciler contract's example record.
const (
	hintLogs = "check `top logs <ns/name>`; last known-good keeps serving"
)

// LeaseConflictError is the run-level typed failure when the reconcile
// lease is held (state-store contract ErrLocked). It unwraps to
// domain.ErrLocked so callers branch with errors.Is; the exact CLI exit
// code is T-310's concern — this is the ExitConflict-shaped, specific-code
// typed error the pipeline hands back. No mutation has happened when it is
// returned, and no operation record is written (the run never started).
type LeaseConflictError struct {
	Lease  string
	Holder string
}

func (e LeaseConflictError) Error() string {
	return domain.LockedError{Lease: e.Lease, Holder: e.Holder}.Error()
}

// Unwrap yields domain.ErrLocked.
func (e LeaseConflictError) Unwrap() error { return domain.ErrLocked }

// RunError wraps a terminal run-level failure with the pipeline phase it
// died in. Unwraps to the underlying cause.
type RunError struct {
	RunID  string // operation id ("op_..." is presentation; raw uuid here)
	Phase  Phase
	Detail string
	Err    error
}

func (e *RunError) Error() string {
	if e.Detail != "" {
		return "reconcile run " + e.RunID + " failed in " + e.Phase.String() + ": " + e.Detail + ": " + e.Err.Error()
	}
	return "reconcile run " + e.RunID + " failed in " + e.Phase.String() + ": " + e.Err.Error()
}

// Unwrap yields the underlying cause.
func (e *RunError) Unwrap() error { return e.Err }

// Phase names one pipeline stage (docs/contracts/reconciler.md), for
// RunError attribution and the README's contract-mapping table.
type Phase string

// IsZero reports whether the phase is unset.
func (p Phase) IsZero() bool { return p == "" }

// String implements fmt.Stringer.
func (p Phase) String() string { return string(p) }

// Pipeline phases in contract order.
const (
	PhaseValidate Phase = "validate"
	PhaseLease    Phase = "lease"
	PhaseOrder    Phase = "order"
	PhaseDiff     Phase = "diff"
	PhaseApply    Phase = "apply"
	PhaseGate     Phase = "gate"
	PhaseRecord   Phase = "record"
	PhasePrune    Phase = "prune"
	PhaseReport   Phase = "report"
)

// IsErrLockedLease reports failure matching the lease conflict shape.
func IsErrLockedLease(err error) bool {
	var lce LeaseConflictError
	return errors.As(err, &lce)
}
