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

// RemoveBackup implements store.Store.RemoveBackup: retention deletes the
// registry row after the engine has removed the file (prune order:
// file first, then row — never a row without its file).
func (s *Store) RemoveBackup(ctx context.Context, path string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE path = ?`, path)
	if err != nil {
		return classify(fmt.Sprintf("remove backup %q", path), err, "backup "+path)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return domain.NewNotFound("backup", path)
	}
	return nil
}

// --- backup registry ------------------------------------------------------

// InsertBackup implements store.Store.InsertBackup.
func (s *Store) InsertBackup(ctx context.Context, b *store.Backup) error {
	if b == nil {
		return fmt.Errorf("insert backup: %w: nil backup", domain.ErrInvalid)
	}
	if b.Path == "" {
		return fmt.Errorf("insert backup: %w: empty path", domain.ErrInvalid)
	}
	createdAt := b.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	} else {
		createdAt = createdAt.UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO backups (path, size, watermark_op, verified_at, verify_result, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		b.Path, b.Size, b.WatermarkOp, timeArg(b.VerifiedAt), b.VerifyResult, createdAt.UnixNano())
	return classify(fmt.Sprintf("insert backup %q", b.Path), err, "backup "+b.Path)
}

// ListBackups implements store.Store.ListBackups (newest first).
func (s *Store) ListBackups(ctx context.Context) ([]*store.Backup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, size, watermark_op, verified_at, verify_result, created_at
		 FROM backups ORDER BY created_at DESC`)
	if err != nil {
		return nil, classify("list backups", err, "")
	}
	defer func() { _ = rows.Close() }()

	out := []*store.Backup{}
	for rows.Next() {
		var (
			b                     store.Backup
			verifiedAt, createdAt sql.NullInt64
			verifyResult          sql.NullString
		)
		if err := rows.Scan(&b.Path, &b.Size, &b.WatermarkOp, &verifiedAt,
			&verifyResult, &createdAt); err != nil {
			return nil, classify("scan backup row", err, "")
		}
		if verifiedAt.Valid {
			b.VerifiedAt = time.Unix(0, verifiedAt.Int64).UTC()
		}
		b.VerifyResult = verifyResult.String
		b.CreatedAt = time.Unix(0, createdAt.Int64).UTC()
		out = append(out, &b)
	}
	if err := rows.Err(); err != nil {
		return nil, classify("list backups", err, "")
	}
	return out, nil
}

// SetBackupVerified implements store.Store.SetBackupVerified.
func (s *Store) SetBackupVerified(ctx context.Context, path, verifyResult string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE backups SET verified_at = ?, verify_result = ? WHERE path = ?`,
		at.UTC().UnixNano(), verifyResult, path)
	if err != nil {
		return classify(fmt.Sprintf("set backup verified %q", path), err, "backup "+path)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return domain.NewNotFound("backup", path)
	}
	return nil
}
