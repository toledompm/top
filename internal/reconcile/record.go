// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"strings"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// recordRun writes the run summary + per-service outcomes in one atomic
// store transaction, advancing per-service applied/known-good state, then
// moves the operation record to its terminal state. The checkpoint merges
// with the existing one — shrinking another service's known-good because
// THIS run did not touch it would erase rollback state.
func (r *Reconciler) recordRun(ctx context.Context, op *domain.Operation, in RunInput, res *RunResult, failureErr string) error {
	green := res.Status == domain.RunSucceeded
	cp := &store.Checkpoint{}
	if prev, err := r.store.GetCheckpoint(ctx); err == nil {
		*cp = *prev
	}
	cp.LastApplied = res.Revision
	if green {
		cp.LastKnownGood = res.Revision
	} else {
		cp.LastKnownGood = "" // never claim repo-wide green on a failed run
	}
	if cp.LastKnownGoodPerService == nil {
		cp.LastKnownGoodPerService = map[domain.ServiceRef]domain.RevisionRef{}
	}
	// Per-service last-known-good advances for every GREEN outcome of this
	// run, even a partially-failed one: the contract's rollback story is
	// per service (the failed service keeps serving its old known-good).
	for _, o := range res.Outcomes {
		if o.Result != domain.OutcomeSucceeded && o.Result != domain.OutcomeNoop {
			continue
		}
		if rr, ok := knownGoodRevision(in, o.Ref); ok {
			cp.LastKnownGoodPerService[o.Ref] = rr
		}
	}
	run := domain.ReconciliationRun{
		OperationID:   op.ID,
		Actor:         op.Actor,
		CorrelationID: op.CorrelationID,
		Revision:      res.Revision,
		Status:        res.Status,
		StartedAt:     firstNonZero(res.StartedAt, op.StartedAt),
		FinishedAt:    firstNonZero(res.FinishedAt, op.FinishedAt),
		Outcomes:      res.Outcomes,
		ValidationErr: res.ValidationErr,
	}
	if err := r.store.RecordReconcileRun(ctx, &run, cp); err != nil {
		return err
	}
	return terminalOperation(ctx, r.store, op, res.Status, failureErr, res.ValidationErr, outcomeErrorTexts(res.Outcomes))
}

// outcomeErrorTexts collects per-service failure texts for the run's
// redacted summary (contract example: the error field explains itself).
func outcomeErrorTexts(outcomes []domain.ServiceOutcome) []string {
	var parts []string
	for _, o := range outcomes {
		if o.Result == domain.OutcomeFailed {
			parts = append(parts, o.Ref.String()+": "+o.Err)
		}
	}
	return parts
}

// knownGoodRevision derives what a green service's last-known-good stores:
// image deploys carry the source-image reference; git builds carry the
// run revision (state-store contract).
func knownGoodRevision(in RunInput, ref domain.ServiceRef) (domain.RevisionRef, bool) {
	for _, svc := range in.Specs {
		if !svc.Ref.Equal(ref) {
			continue
		}
		if svc.Source.Image != "" {
			return domain.RevisionRef{Kind: domain.RevisionImage, Value: svc.Source.Reference()}, true
		}
		return domain.RevisionRef{Kind: domain.RevisionGit, Value: in.Revision}, true
	}
	return domain.RevisionRef{}, false
}

// terminalOperation moves the operation record to its terminal state and
// mirrors it in the store. Failure records REQUIRE non-empty redacted
// text; the failure summary comes from the run's own outcome errors.
func terminalOperation(ctx context.Context, st StateStore, op *domain.Operation, status domain.RunStatus, failureErr, validationErr string, outcomeErrs []string) error {
	if status == domain.RunSucceeded {
		if err := op.Succeed(); err != nil {
			return err
		}
		return st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", op.FinishedAt)
	}
	redacted := firstNonEmpty(failureErr, strings.Join(outcomeErrs, "; "), validationErr, "reconcile run failed")
	if err := op.Fail(redacted); err != nil {
		return err
	}
	return st.UpdateOperationState(ctx, op.ID, domain.OpFailed, op.Err, op.FinishedAt)
}

func firstNonZero(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// recordFailedValidation writes the validate-failure operation + run
// summary (contract: the run stops at validate, records failed with
// per-file errors; previous state keeps running). No lease is held at this
// point — a validation-only run never acquired it.
func (r *Reconciler) recordFailedValidation(ctx context.Context, in RunInput, res *RunResult, verr error) error {
	res.StartedAt = r.clock.Now()
	res.FinishedAt = r.clock.Now()
	op, err := domain.NewOperation(domain.OpReconcile, in.Actor, in.CorrelationID, in.Revision)
	if err != nil {
		return err
	}
	res.OperationID = op.ID
	if err := r.store.InsertOperation(ctx, op); err != nil {
		return err
	}
	if err := r.store.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", res.StartedAt); err != nil {
		return err
	}
	run := domain.ReconciliationRun{
		OperationID:   op.ID,
		Actor:         in.Actor,
		CorrelationID: in.CorrelationID,
		Revision:      in.Revision,
		Status:        domain.RunFailed,
		StartedAt:     res.StartedAt,
		FinishedAt:    res.FinishedAt,
		// Outcomes stay empty: validate failed before any service was
		// touched (the per-file error text carries the why).
		Outcomes:      nil,
		ValidationErr: res.ValidationErr,
	}
	if err := r.store.RecordReconcileRun(ctx, &run, &store.Checkpoint{}); err != nil {
		return err
	}
	if err := op.Fail(res.ValidationErr); err != nil {
		return err
	}
	return r.store.UpdateOperationState(ctx, op.ID, domain.OpFailed, op.Err, op.FinishedAt)
}
