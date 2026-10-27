package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// defaultListLimit applies when ListOperations receives a limit <= 0.
const defaultListLimit = 100

// --- time helpers ---------------------------------------------------------

// timeArg formats a time for an INTEGER (nanoseconds, UTC) column; NULL is
// the stored form of the zero time.
func timeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().UnixNano()
}

// --- operations -----------------------------------------------------------

// InsertOperation implements store.Store.InsertOperation.
func (s *Store) InsertOperation(ctx context.Context, op *domain.Operation) error {
	if op == nil {
		return fmt.Errorf("insert operation: %w: nil operation", domain.ErrInvalid)
	}
	if op.State != domain.OpPending {
		return fmt.Errorf("insert operation %s: %w: new operations must start in %s, got %s",
			op.ID, domain.ErrConflict, domain.OpPending, op.State)
	}
	if !op.Kind.IsValid() || !op.Actor.IsValid() {
		return fmt.Errorf("insert operation %s: %w: invalid kind %q or actor %q",
			op.ID, domain.ErrInvalid, op.Kind, op.Actor)
	}
	createdAt := time.Now().UTC()
	details, err := json.Marshal(op.Details)
	if err != nil {
		return fmt.Errorf("insert operation %s: encode details: %w", op.ID, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO operations
			(id, created_at, kind, state, actor, correlation_id, revision, started_at, finished_at, error, details)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID.String(), createdAt.UnixNano(), op.Kind, op.State, op.Actor,
		op.CorrelationID, op.Revision, timeArg(op.StartedAt), timeArg(op.FinishedAt),
		op.Err, string(details),
	)
	return classify(fmt.Sprintf("insert operation %s", op.ID), err, "operation "+op.ID.String())
}

// UpdateOperationState implements store.Store.UpdateOperationState. It
// follows the *persisted* state (not an in-memory copy), so transitions are
// judged against stored reality and the domain lifecycle
// (domain.CanTransition); invalid moves are ErrConflict.
// started/finished bookkeeping follows the operation's entering time (at).
func (s *Store) UpdateOperationState(ctx context.Context, id uuid.UUID, next domain.OperationState, redactedErr string, at time.Time) error {
	if !next.IsValid() {
		return fmt.Errorf("operation %s: %w: unknown state %q", id, domain.ErrInvalid, next)
	}
	var current domain.OperationState
	err := s.db.QueryRowContext(ctx,
		`SELECT state FROM operations WHERE id = ?`, id.String()).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.NewNotFound("operation", id.String())
	case err != nil:
		return classify(fmt.Sprintf("operation %s", id), err, "operation "+id.String())
	}
	if !domain.CanTransition(current, next) {
		return fmt.Errorf("operation %s: invalid transition %s → %s (terminal states and skips are not allowed): %w",
			id, current, next, domain.ErrConflict)
	}
	if next == domain.OpFailed && redactedErr == "" {
		return fmt.Errorf("operation %s: fail: %w: empty error message (a failure record must explain itself)",
			id, domain.ErrInvalid)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	// started_at: stamped when the operation enters running (kept if already
	// set); finished_at: stamped when it enters a terminal state.
	_, err = s.db.ExecContext(ctx,
		`UPDATE operations SET
			state = ?,
			started_at = CASE WHEN ? = 'running' AND started_at IS NULL THEN ? ELSE started_at END,
			finished_at = CASE WHEN ? IN ('succeeded','failed','aborted') THEN ? ELSE finished_at END,
			error = ?
		 WHERE id = ?`,
		next, next, timeArg(at), next, timeArg(at), redactedErr, id.String())
	return classify(fmt.Sprintf("operation %s → %s", id, next), err, "operation "+id.String())
}

// ListOperations implements store.Store.ListOperations (newest first).
func (s *Store) ListOperations(ctx context.Context, limit int, filter *store.OperationFilter) ([]*domain.Operation, error) {
	q := strings.Builder{}
	q.WriteString(`SELECT id, kind, state, actor, correlation_id, revision,
		started_at, finished_at, error, details
		FROM operations`)
	args := []any{}
	clauses := []string{}
	if filter != nil {
		if filter.State != "" {
			clauses = append(clauses, "state = ?")
			args = append(args, filter.State)
		}
		if filter.Kind != "" {
			clauses = append(clauses, "kind = ?")
			args = append(args, filter.Kind)
		}
		if filter.CorrelationID != "" {
			clauses = append(clauses, "correlation_id = ?")
			args = append(args, filter.CorrelationID)
		}
	}
	if len(clauses) > 0 {
		q.WriteString(" WHERE " + strings.Join(clauses, " AND "))
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	q.WriteString(" ORDER BY created_at DESC, id DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, classify("list operations", err, "")
	}
	defer rows.Close()

	ops := []*domain.Operation{}
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list operations", err, "")
	}
	return ops, nil
}

// scanOperation maps one operations row into a domain.Operation (details
// JSON round-trip included).
func scanOperation(row interface{ Scan(...any) error }) (*domain.Operation, error) {
	var (
		id, kind, state, actor, correlationID, revision, errText string
		startedAt, finishedAt                                    sql.NullInt64
		details                                                  sql.NullString
	)
	if err := row.Scan(&id, &kind, &state, &actor, &correlationID, &revision,
		&startedAt, &finishedAt, &errText, &details); err != nil {
		return nil, classify("scan operation row", err, "")
	}
	op := &domain.Operation{
		Kind:          domain.OperationKind(kind),
		State:         domain.OperationState(state),
		Actor:         domain.Actor(actor),
		CorrelationID: correlationID,
		Revision:      revision,
		Err:           errText,
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("scan operation row: invalid stored id %q: %w", id, domain.ErrCorrupt)
	}
	op.ID = parsed
	if startedAt.Valid {
		op.StartedAt = time.Unix(0, startedAt.Int64).UTC()
	}
	if finishedAt.Valid {
		op.FinishedAt = time.Unix(0, finishedAt.Int64).UTC()
	}
	if details.Valid && details.String != "" && details.String != "{}" {
		if uerr := json.Unmarshal([]byte(details.String), &op.Details); uerr != nil {
			return nil, fmt.Errorf("scan operation %s: corrupt details JSON: %w", id, domain.ErrCorrupt)
		}
	}
	return op, nil
}

// --- observed state -------------------------------------------------------

// UpsertObserved implements store.Store.UpsertObserved.
func (s *Store) UpsertObserved(ctx context.Context, ref domain.ServiceRef, obs *domain.ObservedState, revision string, at time.Time) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("upsert observed: %w: %w", domain.ErrInvalid, err)
	}
	if obs == nil {
		return fmt.Errorf("upsert observed %s: %w: nil snapshot", ref, domain.ErrInvalid)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	encoded, err := json.Marshal(obs)
	if err != nil {
		return fmt.Errorf("upsert observed %s: encode snapshot: %w", ref, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO observed_state (ref, revision, state, observed_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(ref) DO UPDATE SET revision = excluded.revision,
			state = excluded.state, observed_at = excluded.observed_at`,
		ref.String(), revision, string(encoded), at.UTC().UnixNano())
	return classify(fmt.Sprintf("upsert observed %s", ref), err, "observed state for "+ref.String())
}

// GetObserved implements store.Store.GetObserved.
func (s *Store) GetObserved(ctx context.Context, ref domain.ServiceRef) (*domain.ObservedState, string, time.Time, error) {
	var (
		revision, state string
		observedAt      int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT revision, state, observed_at FROM observed_state WHERE ref = ?`,
		ref.String()).Scan(&revision, &state, &observedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, "", time.Time{}, domain.NewNotFound("observed state", ref.String())
	case err != nil:
		return nil, "", time.Time{}, classify(fmt.Sprintf("get observed %s", ref), err, "observed state for "+ref.String())
	}
	obs := &domain.ObservedState{}
	if uerr := json.Unmarshal([]byte(state), obs); uerr != nil {
		return nil, "", time.Time{}, fmt.Errorf("get observed %s: corrupt snapshot JSON: %w", ref, domain.ErrCorrupt)
	}
	return obs, revision, time.Unix(0, observedAt).UTC(), nil
}

// --- checkpoints ----------------------------------------------------------

// SetCheckpoint implements store.Store.SetCheckpoint.
func (s *Store) SetCheckpoint(ctx context.Context, cp *store.Checkpoint) error {
	return persistCP(ctx, s.db, cp)
}

// GetCheckpoint implements store.Store.GetCheckpoint; a fresh database
// yields the zero-valued checkpoint.
func (s *Store) GetCheckpoint(ctx context.Context) (*store.Checkpoint, error) {
	return loadCP(ctx, s.db)
}

// parseCheckpointRow decodes the checkpoint singleton's four columns into a
// store.Checkpoint; unreadable JSON is ErrCorrupt (the store is the source
// of these bytes, so corruption indicates the file is the problem).
func parseCheckpointRow(lastApplied, lastKnownGood, lkgJSON, appliedJSON string) (*store.Checkpoint, error) {
	cp := &store.Checkpoint{
		LastApplied:             lastApplied,
		LastKnownGood:           lastKnownGood,
		LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{},
		AppliedPerService:       map[domain.ServiceRef]string{},
	}
	lkg := map[string]domain.RevisionRef{}
	if err := json.Unmarshal([]byte(lkgJSON), &lkg); err != nil {
		return nil, fmt.Errorf("parse checkpoint: corrupt per-service JSON: %w", domain.ErrCorrupt)
	}
	for k, v := range lkg {
		ref, err := domain.ParseServiceRef(k)
		if err != nil {
			return nil, fmt.Errorf("parse checkpoint: %w", domain.ErrCorrupt)
		}
		cp.LastKnownGoodPerService[ref] = v
	}
	strs := map[string]string{}
	if err := json.Unmarshal([]byte(appliedJSON), &strs); err != nil {
		return nil, fmt.Errorf("parse checkpoint: corrupt applied map JSON: %w", domain.ErrCorrupt)
	}
	for k, v := range strs {
		ref, err := domain.ParseServiceRef(k)
		if err != nil {
			return nil, fmt.Errorf("parse checkpoint: %w", domain.ErrCorrupt)
		}
		cp.AppliedPerService[ref] = v
	}
	return cp, nil
}

// refStrings renders a per-service applied map ("ns/name" keys) for JSON.
func refStrings(in map[domain.ServiceRef]string) map[string]string {
	out := make(map[string]string, len(in))
	for ref, rev := range in {
		out[ref.String()] = rev
	}
	return out
}

// --- webhook deliveries ---------------------------------------------------

// RecordDelivery implements store.Store.RecordDelivery: true = first time
// seen, false = duplicate (contract: INSERT OR IGNORE, zero rows = dup).
func (s *Store) RecordDelivery(ctx context.Context, deliveryID string, at time.Time) (bool, error) {
	if deliveryID == "" {
		return false, fmt.Errorf("record delivery: %w: empty delivery id", domain.ErrInvalid)
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO deliveries (delivery_id, received_at) VALUES (?, ?)`,
		deliveryID, at.UTC().UnixNano())
	if err != nil {
		return false, classify("record delivery", err, "delivery "+deliveryID)
	}
	rows, rerr := res.RowsAffected()
	if rerr != nil {
		rows = 1
	}
	return rows == 1, nil
}
