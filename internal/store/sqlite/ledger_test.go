package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

func TestResourceLedger(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	r := &store.Resource{
		Kind:        "app",
		Identifier:  "default-website",
		Labels:      map[string]string{"top.dev/managed": "true", "top.dev/service": "website"},
		CreatedByOp: "op-123",
		State:       store.ResourceBootstrapOwned,
	}
	if err := st.InsertLedgerEntry(ctx, r); err != nil {
		t.Fatalf("InsertLedgerEntry: %v", err)
	}
	// Duplicate (kind, identifier) is ErrConflict (contract constraint semantics).
	if err := st.InsertLedgerEntry(ctx, r); errOf(err, domain.ErrConflict) {
	} else {
		t.Fatalf("dup entry err = %v, want ErrConflict", err)
	}
	// Same identifier, different kind is a different resource.
	if err := st.InsertLedgerEntry(ctx, &store.Resource{
		Kind: "network", Identifier: "default-website", State: store.ResourceBootstrapOwned,
	}); err != nil {
		t.Fatalf("different kind: %v", err)
	}
	// Invalid state rejected.
	if err := st.InsertLedgerEntry(ctx, &store.Resource{
		Kind: "app", Identifier: "x", State: "bogus",
	}); errOf(err, domain.ErrInvalid) {
	} else {
		t.Fatalf("bogus state err = %v, want ErrInvalid", err)
	}

	// Mark bootstrap-owned → managed.
	if err := st.MarkManaged(ctx, "app", "default-website"); err != nil {
		t.Fatalf("MarkManaged: %v", err)
	}
	if err := st.MarkManaged(ctx, "app", "default-website"); err != nil {
		t.Fatalf("MarkManaged idempotent: %v", err)
	}
	if err := st.MarkManaged(ctx, "app", "ghost"); errOf(err, domain.ErrNotFound) {
	} else {
		t.Fatalf("ghost entry err = %v, want ErrNotFound", err)
	}

	// List, filter, round-trip.
	all, err := st.ListLedger(ctx, "")
	if err != nil {
		t.Fatalf("ListLedger: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListLedger = %d, want 2", len(all))
	}
	apps, _ := st.ListLedger(ctx, "app")
	if len(apps) != 1 || apps[0].State != store.ResourceManaged ||
		apps[0].Labels["top.dev/managed"] != "true" {
		t.Fatalf("app entries: %+v", apps)
	}

	// Remove + removal of a missing entry is ErrNotFound.
	if err := st.RemoveLedgerEntry(ctx, "network", "default-website"); err != nil {
		t.Fatalf("RemoveLedgerEntry: %v", err)
	}
	if err := st.RemoveLedgerEntry(ctx, "network", "default-website"); errOf(err, domain.ErrNotFound) {
	} else {
		t.Fatalf("double-remove err = %v, want ErrNotFound", err)
	}
}

func TestLeaseAcquireReleaseExpire(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()

	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-1", time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire fresh = %v, %v", ok, err)
	}
	// Held by another holder, not expired → ErrLocked with holder identity
	// (contract: persistent contention carries the holder in the message).
	if _, err := st.AcquireLease(ctx, "reconcile", "topctl-1", time.Hour, false); err == nil || !errors.Is(err, domain.ErrLocked) {
		t.Fatalf("contended acquire err = %v, want ErrLocked", err)
	}
	if _, err := st.AcquireLease(ctx, "reconcile", "topctl-1", time.Hour, false); !lockedBy(err, "controller-1") {
		t.Fatalf("contended acquire missing holder identity: %v", err)
	}
	// Holder reacquiring without renew keeps its current expiry.
	expiryBefore := leaseExpiry(t, ctx, st, "reconcile")
	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-1", time.Hour, false); err != nil || !ok {
		t.Fatalf("self acquire = %v, %v", ok, err)
	}
	if got := leaseExpiry(t, ctx, st, "reconcile"); !got.Equal(expiryBefore) {
		t.Fatalf("expiry changed without renew: %s → %s", expiryBefore, got)
	}
	// Renew extends from now: ttl larger than the remaining time, so the
	// expiry must move later.
	after := leaseExpiry(t, ctx, st, "reconcile")
	if ok, err := st.AcquireLease(ctx, "reconcile", "controller-1", 2*time.Hour, true); err != nil || !ok {
		t.Fatalf("renew = %v, %v", ok, err)
	}
	if got := leaseExpiry(t, ctx, st, "reconcile"); !got.After(after) {
		t.Fatalf("renew did not extend: %s → %s", after, got)
	}
	// Release by someone else, un-expired → ErrLocked with holder.
	if err := st.ReleaseLease(ctx, "reconcile", "topctl-1"); err == nil || !errors.Is(err, domain.ErrLocked) || !lockedBy(err, "controller-1") {
		t.Fatalf("foreign release err = %v, want ErrLocked(controller-1)", err)
	}
	// Correct release succeeds; releasing again is a no-op success.
	if err := st.ReleaseLease(ctx, "reconcile", "controller-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.ReleaseLease(ctx, "reconcile", "controller-1"); err != nil {
		t.Fatalf("double release (absent): %v", err)
	}

	// Expired lease: a new holder takes it over without sleeping — the
	// grant expiry is a stored timestamp, so seed one in the past.
	st2 := openTest(t)
	if ok, err := st2.AcquireLease(ctx, "restore", "cli", time.Hour, false); err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	if _, err := st2.AcquireLease(ctx, "restore", "controller", time.Hour, false); err == nil ||
		!errors.Is(err, domain.ErrLocked) || !lockedBy(err, "cli") {
		t.Fatalf("contended restore lease err = %v", err)
	}

	// Same-name lease owned by another identity with tiny ttl: run one
	// acquire against a store whose lease timestamp predates its expiry.
	st3 := openTest(t)
	if ok, err := st3.AcquireLease(ctx, "reconcile", "old", -time.Nanosecond, false); err != nil || !ok {
		t.Fatalf("past-ttl acquire = %v, %v", ok, err)
	}
	// expires_at = now + (-1ns) is in the past → immediately ripe for a
	// takeover by a different holder.
	if ok, err := st3.AcquireLease(ctx, "reconcile", "new", time.Hour, false); err != nil || !ok {
		t.Fatalf("takeover of expired lease = %v, %v; want (true, nil)", ok, err)
	}
}

// lockedBy reports whether err carries the holder identity (ErrLocked
// contract: the holder identity is in the message).
func lockedBy(err error, holder string) bool {
	var le domain.LockedError
	if !errors.Is(err, domain.ErrLocked) {
		return false
	}
	return errors.As(err, &le) && le.Holder == holder
}

// leaseExpiry reads the lease row's expiry via the store's internal db
// handle (same-package test; never a caller pattern) and points errors at
// the given holder-free location with a clean signature.
func leaseExpiry(t *testing.T, ctx context.Context, st *Store, name string) time.Time {
	t.Helper()
	var expiry int64
	if err := st.db.QueryRowContext(ctx, `SELECT expires_at FROM leases WHERE name = ?`, name).Scan(&expiry); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	return time.Unix(0, expiry).UTC()
}

// fmtDelivery renders a per-writer delivery id.
func fmtDelivery(writer, i int) string {
	return fmt.Sprintf("writer-%d-%d", writer, i)
}

// opRecord builds a pending operation row for the contention workload.
func opRecord(id uuid.UUID, writer, i int) *domain.Operation {
	op, err := domain.NewOperation(domain.OpReconcile, domain.ActorUser,
		fmtDelivery(writer, i), "")
	if err != nil {
		panic(err) // static inputs; nothing dynamic to fail on
	}
	op.ID = id
	return op
}

func TestWriteContentionSerializes(t *testing.T) {
	// Two goroutines hammer writes on the shared connection under -race:
	// with MaxOpenConns(1) the pool serializes them, so no SQLITE_BUSY is
	// reachable in-process (documented connection-policy tradeoff). No
	// time.Sleep — contention is exercised, not choreographed.
	st := openTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const writers, perWriter = 2, 50
	var (
		mu   sync.Mutex
		errs []error
	)
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := st.RecordDelivery(ctx, fmtDelivery(writer, i), time.Time{}); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				if err := st.InsertOperation(ctx, opRecord(uuid.New(), writer, i)); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("contention produced errors: %v", errs)
	}
	ops, err := st.ListOperations(ctx, writers*perWriter, nil)
	if err != nil || len(ops) != writers*perWriter {
		t.Fatalf("after contention: %d ops, err=%v; want %d", len(ops), err, writers*perWriter)
	}
}
