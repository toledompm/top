// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// storeLister is the one store read the observe logic performs (ledger
// apps for orphan candidates); narrowing to it keeps the store parameter
// replaceable by the diff path's noLedgerStore adapter.
type storeLister interface {
	ListLedger(ctx context.Context, kind string) ([]*store.Resource, error)
}

// observe gathers the observed state the diff consumes: one Inspect per
// desired service, plus the ownership ledger's app entries so services
// present on the host but absent from Git surface as orphaned (the plan's
// prune candidates live in the same diff — ADR-0002 parity).
//
// A per-service Inspect failure is contained: the service diffs against a
// never-deployed state is WRONG for convergence, so instead its diff is
// suppressed and the service fails the run at apply time with the inspect
// error. A backend-wide unhealthy error aborts the run before any
// mutation (recorded failed, per the contract's no-blind-continue rule).
//
// The body lives in gatherObserved so DiffOnly (read-only diff) and Run
// share ONE implementation (T-309 parity); the method only localizes the
// reconciler's own dependencies (its configured namespaces list).
func (r *Reconciler) observe(ctx context.Context, in RunInput) ([]domain.ServiceRef, map[domain.ServiceRef]domain.ObservedState, error) {
	return gatherObserved(ctx, in, r.backend, r.namesp, r.store)
}

// gatherObserved is the classless form of the observe logic. configuredNss
// is the reconciler's (or CLI's) operator-registered namespace list; nil
// falls back to the platform defaults inside. st is the store whose ledger
// app entries back-derive orphan candidates — its nil case is handled by
// callers (noLedgerStore; Run always has a store).
func gatherObserved(ctx context.Context, in RunInput, be Backend, configuredNss []string, st storeLister) ([]domain.ServiceRef, map[domain.ServiceRef]domain.ObservedState, error) {
	ordered, err := order(in.Specs)
	if err != nil {
		return nil, nil, err
	}
	// Orphan back-derivation prefers the RUN's namespace list, then the
	// Reconciler's configured list (Options), then the platform default.
	nss := in.Namespaces
	if len(nss) == 0 {
		nss = configuredNss
	}
	if len(nss) == 0 {
		nss = config.NamespacePolicyOrder
	}
	obs := make(map[domain.ServiceRef]domain.ObservedState, len(ordered))
	for _, ref := range ordered {
		o, err := be.Inspect(ctx, ref)
		if err != nil {
			if errors.Is(err, domain.ErrUnhealthy) {
				return nil, nil, fmt.Errorf("inspect %s: %w", ref, err)
			}
			if errors.Is(err, domain.ErrNotFound) {
				continue // never deployed: the plan treats absence as create
			}
			return nil, nil, fmt.Errorf("inspect %s: %w", ref, err)
		}
		obs[ref] = o
	}
	// Orphan candidates from the ownership ledger (ADR-0009 layer 3): apps
	// TOP identifies that no spec claims. The plan reports them; deletion
	// happens only via --prune + confirmation AND the two-key gate.
	for _, app := range ledgerApps(ctx, st) {
		ref, ok := refFromAppName(app, nss)
		if !ok {
			continue // foreign naming: never a TOP service identity
		}
		if _, desired := obs[ref]; desired {
			continue
		}
		obs[ref] = domain.ObservedState{Ref: ref, AppName: app, Deployed: true}
	}
	return ordered, obs, nil
}

// ledgerApps lists app-kind ledger identifiers; a ledger failure degrades
// to an empty candidate list WITHOUT masking the diff: prune (executed
// separately) re-checks everything through the two-key gate, so a here-
// missing candidate only loses a *report row*, never a mutation.
func ledgerApps(ctx context.Context, st storeLister) []string {
	entries, err := st.ListLedger(ctx, inventory.KindApp)
	if err != nil {
		return nil
	}
	apps := make([]string, 0, len(entries))
	for _, e := range entries {
		apps = append(apps, e.Identifier)
	}
	return apps
}

// refFromAppName back-derives a ServiceRef from a Dokku app name. Matching
// uses the operator's namespace list; registration happens at install
// time, so an unknown prefix is NOT ours (two-key conservatism).
func refFromAppName(app string, namespaces []string) (domain.ServiceRef, bool) {
	for _, ns := range namespaces {
		rest, ok := strings.CutPrefix(app, ns+"-")
		if ok && rest != "" && domain.ValidName(rest) == nil {
			return domain.Ref(domain.Namespace(ns), domain.Name(rest)), true
		}
	}
	return domain.ServiceRef{}, false
}
