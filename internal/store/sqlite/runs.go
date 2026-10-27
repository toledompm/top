package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// execerQuerier covers *sql.DB and *sql.Tx so checkpoint helpers work
// identically inside and outside transactions.
type execerQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RecordReconcileRun writes one reconciliation run — the run summary row,
// its per-service outcomes, and (optionally) the checkpoint — as a single
// all-or-nothing transaction (the state-store contract's illustrative
// ReconcileRun interaction). Failure rolls back everything: there is no
// half-recorded run in `topctl history`.
//
// For every outcome whose result is succeeded or noop, the run advances that
// service's applied revision to run.Revision (a noop run leaves the working
// baseline at the same revision by definition). cp, when non-nil, replaces
// the checkpoint in the same transaction — the caller computes it, since
// only the reconciler knows the repo-wide semantics of "fully applied".
func (s *Store) RecordReconcileRun(ctx context.Context, run *domain.ReconciliationRun, cp *store.Checkpoint) error {
	if run == nil {
		return fmt.Errorf("record reconcile run: %w: nil run", domain.ErrInvalid)
	}
	if !run.Status.IsValid() {
		return fmt.Errorf("run %s: %w: run status %q", run.OperationID, domain.ErrInvalid, run.Status)
	}
	if run.OperationID == uuid.Nil {
		return fmt.Errorf("record reconcile run: %w: zero operation id", domain.ErrInvalid)
	}
	for _, out := range run.Outcomes {
		if err := out.Validate(); err != nil {
			return fmt.Errorf("run %s: %w", run.OperationID, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return classify("record reconcile run (begin)", err, "")
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	started := run.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs
			(operation_id, actor, correlation_id, revision, status, started_at, finished_at, validation_error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.OperationID.String(), run.Actor, run.CorrelationID, run.Revision,
		run.Status, started.UTC().UnixNano(), timeArg(run.FinishedAt), run.ValidationErr); err != nil {
		return classify(fmt.Sprintf("record run %s", run.OperationID), err, "run "+run.OperationID.String())
	}
	for _, out := range run.Outcomes {
		changes, jerr := json.Marshal(out.Changes)
		if jerr != nil {
			return fmt.Errorf("run %s outcome %s: encode changes: %w", run.OperationID, out.Ref, jerr)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_outcomes
				(operation_id, ref, result, changes, duration_ms, error, hint, skip)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			run.OperationID.String(), out.Ref.String(), out.Result, string(changes),
			out.DurationMs, out.Err, out.Hint, out.Skip); err != nil {
			return classify(fmt.Sprintf("run %s outcome %s", run.OperationID, out.Ref), err, "outcome "+out.Ref.String())
		}
	}
	if cp != nil {
		// Replace the checkpoint first; per-service advances below read and
		// mutate it, so nothing is lost when both are requested.
		if werr := persistCP(ctx, tx, cp); werr != nil {
			return werr
		}
	}
	// Advance per-service applied revisions for succeeded/noop services.
	for _, out := range run.Outcomes {
		if out.Result == domain.OutcomeSucceeded || out.Result == domain.OutcomeNoop {
			current, gerr := loadCP(ctx, tx)
			if gerr != nil {
				return gerr
			}
			if current.AppliedPerService == nil {
				current.AppliedPerService = map[domain.ServiceRef]string{}
			}
			current.AppliedPerService[out.Ref] = run.Revision
			if werr := persistCP(ctx, tx, current); werr != nil {
				return werr
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return classify(fmt.Sprintf("record run %s (commit)", run.OperationID), err, "run "+run.OperationID.String())
	}
	return nil
}

// loadCP reads the checkpoint row into a store.Checkpoint within the given
// query context (zero-valued when the singleton row is absent).
func loadCP(ctx context.Context, q execerQuerier) (*store.Checkpoint, error) {
	var lastApplied, lastKnownGood, lkgJSON, appliedJSON string
	err := q.QueryRowContext(ctx,
		`SELECT last_applied, last_known_good, last_known_good_per_service, applied_per_service
		 FROM checkpoints WHERE id = 1`).
		Scan(&lastApplied, &lastKnownGood, &lkgJSON, &appliedJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return &store.Checkpoint{
			LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{},
			AppliedPerService:       map[domain.ServiceRef]string{},
		}, nil
	}
	if err != nil {
		return nil, classify("read checkpoint", err, "checkpoint")
	}
	return parseCheckpointRow(lastApplied, lastKnownGood, lkgJSON, appliedJSON)
}

// persistCP writes the checkpoint singleton within the given query context.
// lastKnownGoodPerService values are validated (domain.RevisionRef.Validate)
// before marshal — a bad revision ref never reaches disk.
func persistCP(ctx context.Context, q execerQuerier, cp *store.Checkpoint) error {
	if cp == nil {
		return fmt.Errorf("write checkpoint: %w: nil checkpoint", domain.ErrInvalid)
	}
	for ref, rev := range cp.LastKnownGoodPerService {
		if err := rev.Validate(); err != nil {
			return fmt.Errorf("write checkpoint: %v: %w", ref, err)
		}
	}
	lkgJSON, err := json.Marshal(refsToKeys(cp.LastKnownGoodPerService))
	if err != nil {
		return fmt.Errorf("write checkpoint: encode per-service map: %w", err)
	}
	appliedJSON, err := json.Marshal(refStrings(cp.AppliedPerService))
	if err != nil {
		return fmt.Errorf("write checkpoint: encode applied map: %w", err)
	}
	if _, err := q.ExecContext(ctx,
		`INSERT INTO checkpoints
			(id, last_applied, last_known_good, last_known_good_per_service, applied_per_service, updated_at)
		 VALUES (1, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			last_applied = excluded.last_applied,
			last_known_good = excluded.last_known_good,
			last_known_good_per_service = excluded.last_known_good_per_service,
			applied_per_service = excluded.applied_per_service,
			updated_at = excluded.updated_at`,
		cp.LastApplied, cp.LastKnownGood, string(lkgJSON), string(appliedJSON), time.Now().UTC().UnixNano()); err != nil {
		return classify("write checkpoint", err, "checkpoint")
	}
	return nil
}

// refsToKeys flattens ServiceRef keys to "ns/name" strings for JSON.
func refsToKeys(refs map[domain.ServiceRef]domain.RevisionRef) map[string]domain.RevisionRef {
	out := make(map[string]domain.RevisionRef, len(refs))
	for ref, rev := range refs {
		out[ref.String()] = rev
	}
	return out
}
