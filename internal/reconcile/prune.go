// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"fmt"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// pruneGateKey is the ledger kind behind app deletions.
const pruneGateKey = inventory.KindApp

// pruneStep executes the delete-decision enforcement for a run that got
// here WITH the consent pair (--prune + --confirm, checked in validate).
// It walks the plan's delete entries and, for each, asks the two-key gate
// (ledger row owned AND fresh inventory snapshot agrees, ADR-0009 layer
// 3); only a passing gate reaches Backend.Destroy, followed by the ledger
// drop. Refusals are recorded per candidate with their reason — prune
// produces decisions, and only what the gate permitted is deleted.
func (r *Reconciler) pruneStep(ctx context.Context, in RunInput, pl domain.Plan, res *RunResult) {
	for _, sp := range pl.Services {
		if sp.Kind != domain.PlanDelete {
			continue
		}
		deleteOne(ctx, r, sp.Ref, res)
	}
}

// deleteOne executes one gate-checked deletion.
func deleteOne(ctx context.Context, r *Reconciler, ref domain.ServiceRef, res *RunResult) {
	app, _ := domain.AppName(ref.Namespace, ref.Name)
	if r.pruneGate == nil {
		res.PruneRefusals = append(res.PruneRefusals, PruneRefusal{
			Ref: ref, Reason: "no prune gate configured: deletes are refused without the two-key proof (ADR-0009)",
		})
		return
	}
	allowed, reason, err := r.pruneGate.CanDelete(ctx, ref)
	if err != nil {
		res.PruneRefusals = append(res.PruneRefusals, PruneRefusal{
			Ref: ref, Reason: "gate error: " + err.Error(),
		})
		return
	}
	if !allowed {
		res.PruneRefusals = append(res.PruneRefusals, PruneRefusal{Ref: ref, Reason: reason})
		return
	}
	if err := r.backend.Destroy(ctx, ref); err != nil {
		res.PruneRefusals = append(res.PruneRefusals, PruneRefusal{
			Ref: ref, Reason: "destroy failed: " + err.Error(),
		})
		return
	}
	if err := r.store.RemoveLedgerEntry(ctx, pruneGateKey, app); err != nil {
		// The app is gone but the ledger row lingers: a real inconsistency
		// to surface, not to hide — but it must not mask the deletion
		// itself from the report.
		res.PruneRefusals = append(res.PruneRefusals, PruneRefusal{
			Ref: ref, Reason: fmt.Sprintf("deleted %s but ledger drop failed: %v", app, err),
		})
		return
	}
	res.Pruned = append(res.Pruned, ref)
}

// LedgerPruneGate is the default two-key gate: it delegates the decision
// to inventory.Deletable (an owned ledger row plus a fresh snapshot that
// still agrees), so the reconciler never invents its own ownership proof.
type LedgerPruneGate struct {
	St         store.Store
	Snap       SnapshotFunc
	Namespaces []string
}

// CanDelete applies the two-key rule to the service's derived app.
func (g LedgerPruneGate) CanDelete(ctx context.Context, ref domain.ServiceRef) (bool, string, error) {
	if g.Snap == nil {
		return false, "no snapshot provider: refusing deletes without fresh host proof (ADR-0009)", nil
	}
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		return false, fmt.Sprintf("service %s: %v", ref, err), nil
	}
	snap, err := g.Snap(ctx)
	if err != nil {
		return false, "", fmt.Errorf("fresh inventory snapshot: %w", err)
	}
	dec, err := inventory.Deletable(ctx, g.St, pruneGateKey, app, snap, g.Namespaces)
	if err != nil {
		return false, "", err
	}
	if !dec.Deletable {
		return false, dec.String(), nil
	}
	return true, "", nil
}
