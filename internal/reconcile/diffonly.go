// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"fmt"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/plan"
	"github.com/toledompm/top/internal/store"
)

// DiffOnly is the T-309 read-only diff path: THE same pipeline phases
// (validate → inspect → plan → DiffReport) minus every mutating or
// record-creating stage. `topctl diff` (docs/contracts/cli.md "zero side
// effects") consumes it — it never re-implements ComputeAll on its own, so
// the run and the printed diff can never diverge (reconciler.md "Diff
// output (shared with `topctl diff`)", ADR-0002 parity).
//
// Deliberate differences from Run:
//   - no lease (a read never serializes against writers),
//   - no operation record, no checkpoint, no apply, no gate, no prune,
//   - prune is NOT allowed in the plan: delete rows are `delete(prune
//     only)` per the contract and `diff` has no --prune flag.
//
// st may be nil (a host without a state store yet): the ledger's orphan
// candidates then read as "none" — the same degraded behavior Run's
// ledgerApps applies on a ledger failure, chosen here so a fresh host gets
// its diff without the CLI inventing infrastructure. A noLedgerStore keeps
// observe's ledger call type-safe on absent state.
func DiffOnly(ctx context.Context, in RunInput, be Backend, st StateStore) (DiffReport, error) {
	if err := ctx.Err(); err != nil {
		return DiffReport{}, err
	}
	if be == nil {
		return DiffReport{}, fmt.Errorf("%w: DiffOnly: Backend is required", domain.ErrInvalid)
	}
	// The validate phase is the SAME phase Run runs (strict; a validation
	// failure fails the whole read before any Inspect, mirroring the
	// contract's "one bad spec must not take down the fleet" — for a read
	// it means the diff refuses to render half a fleet).
	if verr := validateRun(in); verr != nil {
		return DiffReport{}, verr
	}
	_, obs, err := gatherObserved(ctx, in, be, nil, noLedgerStore{st})
	if err != nil {
		return DiffReport{}, err
	}
	pl := plan.ComputeAll(in.Specs, obs, plan.WithSelfService(SelfRef), plan.WithPruneAllowed(in.Prune && in.Confirm))
	pl.Revision = in.Revision
	return buildDiffReport(pl, in), nil
}

// noLedgerStore wraps a possibly-nil store for the DIFF's ledger read:
// a nil inner store reports "no orphan candidates at all" (the same truth
// Run concludes when the ledger is unreadable), and any OTHER method call
// panics loudly — a programming error, never a silent zero. A non-nil
// inner value is passed through untouched.
type noLedgerStore struct {
	inner store.Store
}

// ListLedger implements the one read the diff path performs; it swallows
// the nil case so a fresh host diffs without provisioning a state file.
func (s noLedgerStore) ListLedger(ctx context.Context, kind string) ([]*store.Resource, error) {
	if s.inner == nil {
		return nil, nil
	}
	return s.inner.ListLedger(ctx, kind)
}
