package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/toledompm/top/internal/domain"
)

// --- serialization lease ---------------------------------------------------

// AcquireLease implements store.Store.AcquireLease. The whole
// expire-check-and-grant runs in one transaction whose first statement is a
// DELETE — an immediate write, so the write lock is held across the
// decision: two processes can never both judge a contested lease free.
// busy_timeout covers waiting for the lock; persistent contention surfaces
// as ErrLocked.
//
// Legality: a lease held by another identity that has not expired →
// ErrLocked with the holder identity; no row or an expired row → grant with
// the new expiry; held by the caller → grant keeping the current expiry
// when renew is false, extending by ttl from now when renew is true.
func (s *Store) AcquireLease(ctx context.Context, name, holderID string, ttl time.Duration, renew bool) (bool, error) {
	if name == "" || holderID == "" {
		return false, fmt.Errorf("acquire lease: %w: empty name or holder", domain.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, classify(fmt.Sprintf("acquire lease %q (begin)", name), err, "lease "+name)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	now := time.Now().UTC()
	// First statement is a write: it takes the DB write lock immediately,
	// and conveniently performs the expired-lease takeover purge.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM leases WHERE name = ? AND expires_at <= ?`, name, now.UnixNano()); err != nil {
		return false, classify(fmt.Sprintf("acquire lease %q (expire)", name), err, "lease "+name)
	}
	var holder string
	var expires int64
	err = tx.QueryRowContext(ctx,
		`SELECT holder_id, expires_at FROM leases WHERE name = ?`, name).
		Scan(&holder, &expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// free — grant below
	case err != nil:
		return false, classify(fmt.Sprintf("acquire lease %q", name), err, "lease "+name)
	case holder != holderID && expires > now.UnixNano():
		return false, fmt.Errorf("acquire lease: %w", domain.LockedError{Lease: name, Holder: holder})
	}

	newExpiry := now.Add(ttl).UnixNano()
	// By construction the conflicting row (if any) is either our own or was
	// purged above. Re-granting our own lease inside the same write-locked
	// transaction cannot be observed by another acquirer: renew=false keeps
	// the current expiry, renew=true extends it.
	if holder == holderID && !renew {
		newExpiry = expires
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO leases (name, holder_id, acquired_at, expires_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
			holder_id = excluded.holder_id,
			expires_at = excluded.expires_at`,
		name, holderID, now.UnixNano(), newExpiry); err != nil {
		return false, classify(fmt.Sprintf("acquire lease %q", name), err, "lease "+name)
	}
	if err := tx.Commit(); err != nil {
		return false, classify(fmt.Sprintf("acquire lease %q (commit)", name), err, "lease "+name)
	}
	return true, nil
}

// ReleaseLease implements store.Store.ReleaseLease. Releasing an un-expired
// lease held by someone else is ErrLocked (with that holder in the
// message); releasing an expired or absent lease is a no-op success.
func (s *Store) ReleaseLease(ctx context.Context, name, holderID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM leases
		 WHERE name = ? AND (holder_id = ? OR expires_at <= ?)`,
		name, holderID, time.Now().UTC().UnixNano())
	if err != nil {
		return classify(fmt.Sprintf("release lease %q", name), err, "lease "+name)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Nothing was deleted: either absent (fine) or held by someone
		// else with time left — surface whose it is.
		var holder string
		qerr := s.db.QueryRowContext(ctx,
			`SELECT holder_id FROM leases WHERE name = ?`, name).Scan(&holder)
		if errors.Is(qerr, sql.ErrNoRows) {
			return nil
		}
		if qerr != nil {
			return classify(fmt.Sprintf("release lease %q", name), qerr, "lease "+name)
		}
		return fmt.Errorf("release lease: %w", domain.LockedError{Lease: name, Holder: holder})
	}
	return nil
}
