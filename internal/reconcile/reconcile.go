// SPDX-License-Identifier: MIT

// Package reconcile implements the reconciler pipeline of
// docs/contracts/reconciler.md: turn (desired, observed) into operations,
// model decisions per ADR-0002, trigger ownership per ADR-0003.
//
// Pipeline (one Run, phases in contract order):
//
//	validate (strict; a validation failure fails the WHOLE run before any
//	          mutation and records per-file errors)
//	→ lease  (SQLite serialization lease; another writer holding it is a
//	          typed LeaseConflictError — never apply while held)
//	→ order  (namespace tier core → observability → rest, then dependsOn)
//	→ diff   (internal/plan's zero-side-effect Compute — the same plan
//	          `topctl diff` prints; ADR-0002 parity)
//	→ apply  (per service, errors contained per service; a failing service
//	          records failed, its dependents skip with dependency-failed,
//	          independent services continue)
//	→ gate   (readiness: Inspect until deployed+running under a bounded
//	          deadline; the checks-based gate (T-307/T-308) plugs in via
//	          the Backend seam next wave)
//	→ record (operation record + run summary with per-service outcomes;
//	          last-known-good per service when green)
//	→ prune  (only with --prune AND --confirm — the ADR-0009 destructive
//	          consent pair; the two-key gate decides; the reconciler never
//	          deletes directly, decisions only)
//	→ report (RunResult carries the plan so topctl diff (T-309) reuses it)
//
// Spec parsing is NOT this package's job: internal/spec produces the
// Services; internal/gitsource hands the caller the parsed bundle and
// revision (RunInput). This package is host- and store-mutating through
// two seams only: Backend and StateStore (see seam.go).
//
// Special cases honored (ADR-0005/0006): the controller-self service
// (core namespace, name top-controller) is adoption-aware (T-402): before
// the ownership handoff its drift yields a skipped outcome with reason
// self-drift-reported; after it the adopted self converges field drift
// like any other service, while a revision/image drift stays the
// human-gated deploy (adopt.go).
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/plan"
)

// SelfRef is TOP's own controller service (ADR-0005/0006): specs in the
// core namespace resolving to this ref are special-cased — report-only
// until the bootstrap handoff (T-402, adoption in the ledger), then
// field-converging with the image/revision change still human-gated.
var SelfRef = domain.Ref(domain.CoreNamespace, "top-controller")

// LeaseName is the reconcile run's serialization-lease name (one reconcile
// at a time across actors).
const LeaseName = "reconcile"

// LeaseTTL is how long a holder keeps the lease absent renewal; a crashed
// holder's lease expires after this, unblocking reconciliation.
const LeaseTTL = 10 * time.Minute

// Default readiness pacing (bounded wait per the contract, e.g. 3 min).
const (
	DefaultReadinessDeadline  = 3 * time.Minute
	DefaultReadinessPollEvery = 5 * time.Second
)

// SourcePos is where a resolved service came from: repo-relative spec file
// and the 1-based document line (0 when unknown). Carried by the caller —
// the spec/git sides own positions; the reconciler surfaces it in
// validation failures and the diff report (T-309).
type SourcePos struct {
	File string
	Line int
}

// String renders "file:line" ("file" when Line is 0).
func (p SourcePos) String() string {
	if p.Line > 0 {
		return fmt.Sprintf("%s:%d", p.File, p.Line)
	}
	return p.File
}

// ServiceDiff is one report row for `topctl diff` (T-309 next wave): the
// per-service plan plus the spec source it came from. Values only — the
// same FieldChange objects the executed plan carries, so reports can never
// diverge from what apply executed.
type ServiceDiff struct {
	Ref    domain.ServiceRef
	Kind   domain.ChangeKind
	Source SourcePos // spec origin; zero when unknown
	// Changes are the field-level entries (internal/domain/plan.go).
	Changes []domain.FieldChange
}

// DiffReport is the shared report for T-309: per-service diffs with
// sources, plus the orphaned report.
type DiffReport struct {
	Revision string
	Services []ServiceDiff
	Orphaned []domain.ServiceRef
}

// RunInput is everything a run needs. The spec bundle is already parsed
// and strict-validated by internal/spec — the caller hands the resolved
// services and the resolved revision (NOT git, NOT disk).
type RunInput struct {
	// Specs are the resolved services of this run.
	Specs []domain.Service
	// Revision is the resolved revision this run reconciles toward
	// (a full git sha — ADR-0002; DesiredState.Validate enforces it).
	Revision string
	// Actor is what triggered the run (user/controller/webhook).
	Actor domain.Actor
	// CorrelationID ties the run to its trigger (never a secret).
	CorrelationID string
	// HolderID names the lease holder in the state store.
	HolderID string
	// Prune is the CLI `--prune`: delete services absent from Git. The
	// consent PAIR is `--prune` + `--confirm` (ADR-0009): Prune without
	// Confirm is a fail-fast run error before anything happens.
	Prune bool
	// Confirm is the `--confirm` side of the destructive pair; it gates
	// the whole run's destructive actions (today: prune deletions).
	Confirm bool
	// SelfDeployApproved is the ADR-0005 human gate for the
	// controller-self app (T-406): only the explicit-human CLI path —
	// `topctl deploy core/top-controller`, behind the confirm gate — sets
	// it. Poll runs, webhook runs, and the controller loops never do.
	// When true AND this run saw the adopted (managed) self ledger row,
	// a revision/image change on the controller is APPLIED (the human
	// takeover of what the reconciler otherwise only reports); every
	// other run keeps the report-only human-gate posture. Validate
	// refuses the flag for any actor other than user.
	SelfDeployApproved bool
	// Namespaces are the operator-registered namespaces (ownership
	// naming + prune two-key rule). Empty → config.NamespacePolicyOrder
	// inside the default prune gate.
	Namespaces []string
	// VolumeRoot roots the TOP-managed host directories storage entries
	// point at ("<VolumeRoot>/<app>-<entry>" where app is
	// "<namespace>-<name>"). Storage changes without a VolumeRoot fail
	// contained — TOP never mounts un-owned host dirs (ADR-0009).
	VolumeRoot string
	// Sources maps a service ref to its spec origin for error/diff
	// reporting; optional.
	Sources map[domain.ServiceRef]SourcePos
}

// RunResult is the run's full report: per-service outcomes, the executed
// plan, prune decisions, and timing. It carries the diff for T-309.
type RunResult struct {
	// OperationID ties the run to its append-only operation record.
	OperationID uuid.UUID
	// Revision the run reconciled toward.
	Revision string
	// Status of the overall run.
	Status domain.RunStatus
	// Outcomes per service, in the pipeline's execution order.
	Outcomes []domain.ServiceOutcome
	// Plan is the executed zero-side-effect diff (internal/plan); its
	// revision field is set.
	Plan domain.Plan
	// Orphaned lists services present on the host but absent from Git
	// (reported even without --prune, per the contract).
	Orphaned []domain.ServiceRef
	// Pruned lists services actually removed by this run.
	Pruned []domain.ServiceRef
	// PruneRefusals are orphan candidates the two-key gate refused, and
	// why (ADR-0009 skip-with-reason, never a bare failure).
	PruneRefusals []PruneRefusal
	// DiffReport assembles the per-service diff with spec sources.
	DiffReport DiffReport
	// SelfAdopted reports whether THIS run saw the controller-self app's
	// ownership row in state managed — the ADR-0005 bootstrap handoff
	// recorded (T-402). It is the evidence the adoption-aware self
	// posture was judged from (report-only pre-handoff, human-gated
	// image drift, field-only convergence otherwise).
	SelfAdopted bool
	// StartedAt/FinishedAt read from the injected Clock.
	StartedAt  time.Time
	FinishedAt time.Time
	// ValidationErr carries the joined validate errors when the run died
	// in validate ("" otherwise).
	ValidationErr string
}

// PruneRefusal records why a candidate deletion was refused.
type PruneRefusal struct {
	Ref    domain.ServiceRef
	Reason string
}

// SnapshotFunc captures a fresh host inventory snapshot for the default
// prune gate's second key (ADR-0009). The caller supplies it (only the
// caller's side has the exec runner); nil means prune candidates are
// refused with a reason.
type SnapshotFunc func(ctx context.Context) (*inventory.HostInventory, error)

// Reconciler drives runs. Build with New; Run is the single entry point
// (RunInput is the current shape; T-310 owns the final caller canon).
type Reconciler struct {
	backend    Backend
	store      StateStore
	clock      Clock
	secrets    SecretResolver
	pruneGate  PruneGate
	namesp     []string
	volumeRoot string
	readyGate  readinessPolicy
}

// readinessPolicy is the gate's pacing (Clock-injected: no sleeps in
// tests, no fake time in prod).
type readinessPolicy struct {
	deadline time.Duration
	every    time.Duration
}

// Options builds a Reconciler. Backend and Store are required; everything
// else has defaults.
type Options struct {
	Backend Backend
	Store   StateStore
	// Clock defaults to WallClock.
	Clock Clock
	// Secrets is optional; a service with secret-ref changes and no
	// resolver fails contained with a clear error text.
	Secrets SecretResolver
	// PruneGate defaults to LedgerPruneGate when Snapshot is given.
	PruneGate PruneGate
	// Snapshot enables the default LedgerPruneGate.
	Snapshot SnapshotFunc
	// Namespaces pass to the default prune gate (ADR-0009 naming).
	Namespaces []string
	// VolumeRoot roots storage host directories.
	VolumeRoot string
	// Readiness gate pacing; zero → package defaults.
	ReadinessDeadline time.Duration
	ReadinessPoll     time.Duration
}

// New validates the required seams and applies defaults.
func New(o Options) (*Reconciler, error) {
	if o.Backend == nil {
		return nil, fmt.Errorf("%w: reconcile: Options.Backend is required", domain.ErrInvalid)
	}
	if o.Store == nil {
		return nil, fmt.Errorf("%w: reconcile: Options.Store is required", domain.ErrInvalid)
	}
	clock := o.Clock
	if clock == nil {
		clock = WallClock{}
	}
	gate := readinessPolicy{
		deadline: o.ReadinessDeadline,
		every:    o.ReadinessPoll,
	}
	if gate.deadline == 0 {
		gate.deadline = DefaultReadinessDeadline
	}
	if gate.every == 0 {
		gate.every = DefaultReadinessPollEvery
	}
	var pgate PruneGate
	if o.PruneGate != nil {
		pgate = o.PruneGate
	} else if o.Snapshot != nil {
		pgate = LedgerPruneGate{St: o.Store, Snap: o.Snapshot, Namespaces: o.Namespaces}
	}
	return &Reconciler{
		backend:    o.Backend,
		store:      o.Store,
		clock:      clock,
		secrets:    o.Secrets,
		pruneGate:  pgate,
		namesp:     o.Namespaces,
		volumeRoot: o.VolumeRoot,
		readyGate:  gate,
	}, nil
}

// Run executes one pipeline pass (docs/contracts/reconciler.md).
//
// Error policy: Run always reports its full RunResult; the returned error
// is reserved for RUN-LEVEL terminal conditions —
//
//   - validation failure (ErrInvalid-class, per-file text; the run is
//     recorded failed before ANY mutation),
//   - lease conflict (LeaseConflictError; the run never started, so no
//     record is written — the caller surfaces the conflict code),
//   - prune consent refusal (`--prune` without `--confirm`),
//   - store failures (RunError, phase-tagged).
//
// Everything else — apply failures, readiness misses, prune gate
// refusals — is CONTAINED in per-service outcomes; RunStatus says so.
func (r *Reconciler) Run(ctx context.Context, in RunInput) (RunResult, error) {
	if err := ctx.Err(); err != nil {
		return RunResult{}, err
	}
	res := RunResult{Revision: in.Revision}
	res.DiffReport.Revision = in.Revision

	// --- validate (first phase: before lease, before any mutation) ---------
	if verr := validateRun(in); verr != nil {
		res.Status = domain.RunFailed
		res.ValidationErr = verr.Error()
		if in.Prune && !in.Confirm {
			verr = errors.Join(verr, fmt.Errorf(
				"prune is destructive: pass --confirm together with --prune (ADR-0009 consent pair)"))
		}
		if recErr := r.recordFailedValidation(ctx, in, &res, verr); recErr != nil {
			return res, wrapRun(res.OperationID, PhaseRecord, recErr)
		}
		return res, verr
	}
	if in.Prune && !in.Confirm {
		// Consent pair on its own (validate alone cannot express it).
		return res, fmt.Errorf(
			"%w: prune is destructive: pass --confirm together with --prune (ADR-0009 consent pair)",
			domain.ErrInvalid)
	}

	// --- lease -----------------------------------------------------------------
	holder := in.HolderID
	if holder == "" {
		holder = "top-reconciler"
	}
	if _, err := r.store.AcquireLease(ctx, LeaseName, holder, LeaseTTL, false); err != nil {
		return res, wrapLocked(err)
	}
	defer func() {
		// Release must never mask the run's own result: an unreleased
		// lease expires after the TTL either way.
		_ = r.store.ReleaseLease(context.WithoutCancel(ctx), LeaseName, holder)
	}()

	// --- operation record: pending → running during work ------------------------
	res.StartedAt = r.clock.Now()
	op, opErr := domain.NewOperation(domain.OpReconcile, in.Actor, in.CorrelationID, in.Revision)
	if opErr != nil {
		return res, opErr
	}
	res.OperationID = op.ID
	if err := r.store.InsertOperation(ctx, op); err != nil {
		return res, wrapRun(op.ID, PhaseRecord, err)
	}
	if err := op.Start(); err != nil {
		return res, wrapRun(op.ID, PhaseRecord, err)
	}
	if err := r.store.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", r.clock.Now()); err != nil {
		return res, wrapRun(op.ID, PhaseRecord, err)
	}

	// --- order + diff ------------------------------------------------------------
	ordered, obs, obsErr := r.observe(ctx, in)
	if obsErr != nil {
		res.Status = domain.RunFailed
		res.FinishedAt = r.clock.Now()
		recErr := r.recordRun(ctx, op, in, &res, obsErr.Error())
		if recErr != nil {
			return res, wrapRun(op.ID, PhaseRecord, recErr)
		}
		return res, wrapRun(op.ID, PhaseDiff, obsErr)
	}

	planOpts := []plan.Option{plan.WithSelfService(SelfRef)}
	if in.Prune { // consent pair already validated above
		planOpts = append(planOpts, plan.WithPruneAllowed(true))
	}
	pl := plan.ComputeAll(in.Specs, obs, planOpts...)
	pl.Revision = in.Revision
	res.Plan = pl
	res.Orphaned = pl.Orphaned
	res.DiffReport = buildDiffReport(pl, in)

	// T-402: the controller-self special case is adoption-aware. The
	// ownership ledger decides once per run (adopt.go): with the
	// bootstrap handoff recorded the adopted self converges field
	// drift; a revision change and pre-adoption states stay
	// report-only / human-gated (ADR-0005).
	selfAdopted, selfAdoptErr := adoptedSelf(ctx, r.store)
	res.SelfAdopted = selfAdopted

	// --- apply + gate (per service, contained errors) ------------------------------
	// WithOpID threads the operation id to ledger attribution.
	r.applyAndGate(WithOpID(ctx, op.ID.String()), in, ordered, pl, selfAdopted, selfAdoptErr, &res)
	res.Status = runStatus(res.Outcomes)
	if ctx.Err() != nil {
		// A cancelled run is failed even when the services that ran were
		// green: it stopped mid-pipeline and recorded nothing for the rest.
		res.Status = domain.RunFailed
	}

	// --- prune -----------------------------------------------------------------------
	if in.Prune {
		r.pruneStep(ctx, in, pl, &res)
	}

	// --- record (run summary + checkpoint; operation terminal) -------------------------
	res.FinishedAt = r.clock.Now()
	if err := r.recordRun(ctx, op, in, &res, ""); err != nil {
		return res, wrapRun(op.ID, PhaseRecord, err)
	}
	return res, runErrForStatus(res)
}

// runStatus derives the overall status: failed when any service outcome
// failed; succeeded otherwise (skips, orphan reports, and prune refusals
// do not fail a run — every real failure records an explicit failed
// outcome).
func runStatus(outcomes []domain.ServiceOutcome) domain.RunStatus {
	for _, o := range outcomes {
		if o.Result == domain.OutcomeFailed {
			return domain.RunFailed
		}
	}
	return domain.RunSucceeded
}

// runErrForStatus attaches a terminal error on the caller-facing channel
// when the run failed at service level (contract: run state failed).
func runErrForStatus(res RunResult) error {
	if res.Status != domain.RunFailed {
		return nil
	}
	return wrapRun(res.OperationID, PhaseApply, errors.New(failedOutcomeText(res.Outcomes)))
}

func failedOutcomeText(outcomes []domain.ServiceOutcome) string {
	var parts []string
	for _, o := range outcomes {
		if o.Result == domain.OutcomeFailed {
			parts = append(parts, o.Ref.String()+": "+o.Err)
		}
	}
	return strings.Join(parts, "; ")
}

// wrapRun stamps a RunError carrying the operation id and phase.
func wrapRun(opID uuid.UUID, phase Phase, err error) error {
	return &RunError{RunID: "op_" + opID.String(), Phase: phase, Err: err}
}

// wrapLocked maps a store lease failure onto typed LeaseConflictError
// (ExitConflict-shaped for the caller; T-310 owns the exit-code canon).
func wrapLocked(err error) error {
	if !errors.Is(err, domain.ErrLocked) {
		return err
	}
	var le domain.LockedError
	if errors.As(err, &le) {
		return LeaseConflictError{Lease: le.Lease, Holder: le.Holder}
	}
	return LeaseConflictError{Lease: LeaseName, Holder: "unknown"}
}
