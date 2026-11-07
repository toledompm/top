package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// --- resource ledger (ADR-0009) -------------------------------------------

// InsertLedgerEntry implements store.Store.InsertLedgerEntry.
func (s *Store) InsertLedgerEntry(ctx context.Context, r *store.Resource) error {
	if r == nil {
		return fmt.Errorf("insert ledger entry: %w: nil resource", domain.ErrInvalid)
	}
	if strings.TrimSpace(r.Identifier) == "" || r.Kind == "" {
		return fmt.Errorf("insert ledger entry: %w: empty kind or identifier", domain.ErrInvalid)
	}
	if !r.State.IsValid() {
		return fmt.Errorf("insert ledger entry %s %q: %w: unknown resource state %q",
			r.Kind, r.Identifier, domain.ErrInvalid, r.State)
	}
	createdAt := r.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	} else {
		createdAt = createdAt.UTC()
	}
	labels, err := json.Marshal(r.Labels)
	if err != nil {
		return fmt.Errorf("insert ledger entry %s %q: encode labels: %w", r.Kind, r.Identifier, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO resources (kind, identifier, labels, created_by_op, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.Kind, r.Identifier, string(labels), r.CreatedByOp, r.State, createdAt.UnixNano())
	return classify(fmt.Sprintf("insert ledger entry %s %q", r.Kind, r.Identifier),
		err, r.Kind+"/"+r.Identifier)
}

// MarkManaged implements store.Store.MarkManaged: the bootstrap-owned →
// managed handoff (ADR-0009). Unknown entries are ErrNotFound; entries
// marked pre-existing are ErrConflict — adoption goes through the
// bootstrap inventory, never silently by a state bump.
func (s *Store) MarkManaged(ctx context.Context, kind, identifier string) error {
	var state store.ResourceState
	err := s.db.QueryRowContext(ctx,
		`SELECT state FROM resources WHERE kind = ? AND identifier = ?`,
		kind, identifier).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.NewNotFound("ledger entry", kind+"/"+identifier)
	case err != nil:
		return classify(fmt.Sprintf("mark managed %s %q", kind, identifier), err, kind+"/"+identifier)
	}
	if state == store.ResourcePreExisting {
		return fmt.Errorf("ledger entry %s/%s: pre-existing resources are adopted only via the bootstrap inventory: %w",
			kind, identifier, domain.ErrConflict)
	}
	if state == store.ResourceManaged {
		return nil // idempotent: already managed
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE resources SET state = ? WHERE kind = ? AND identifier = ?`,
		store.ResourceManaged, kind, identifier); err != nil {
		return classify(fmt.Sprintf("mark managed %s %q", kind, identifier), err, kind+"/"+identifier)
	}
	return nil
}

// ListLedger implements store.Store.ListLedger (kind "" = every kind),
// oldest first so install-order reads back naturally.
func (s *Store) ListLedger(ctx context.Context, kind string) ([]*store.Resource, error) {
	q := `SELECT kind, identifier, labels, created_by_op, state, created_at FROM resources`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY created_at ASC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, classify("list ledger", err, "")
	}
	defer func() { _ = rows.Close() }()

	out := []*store.Resource{}
	for rows.Next() {
		var r store.Resource
		var labels string
		var createdAt int64
		if err := rows.Scan(&r.Kind, &r.Identifier, &labels, &r.CreatedByOp, &r.State, &createdAt); err != nil {
			return nil, classify("scan ledger row", err, "")
		}
		r.State = store.ResourceState(r.State)
		if !r.State.IsValid() {
			return nil, fmt.Errorf("scan ledger row %s %q: corrupt state %q: %w",
				r.Kind, r.Identifier, r.State, domain.ErrCorrupt)
		}
		r.Labels = map[string]string{}
		if labels != "" && labels != "{}" {
			if uerr := json.Unmarshal([]byte(labels), &r.Labels); uerr != nil {
				return nil, fmt.Errorf("scan ledger row %s %q: corrupt labels JSON: %w",
					r.Kind, r.Identifier, domain.ErrCorrupt)
			}
		}
		r.CreatedAt = time.Unix(0, createdAt).UTC()
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list ledger", err, "")
	}
	return out, nil
}

// RemoveLedgerEntry implements store.Store.RemoveLedgerEntry.
func (s *Store) RemoveLedgerEntry(ctx context.Context, kind, identifier string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM resources WHERE kind = ? AND identifier = ?`, kind, identifier)
	if err != nil {
		return classify(fmt.Sprintf("remove ledger entry %s %q", kind, identifier), err, kind+"/"+identifier)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return domain.NewNotFound("ledger entry", kind+"/"+identifier)
	}
	return nil
}
