// SPDX-License-Identifier: MIT

package reconcile

import (
	"errors"
	"fmt"
	"sort"

	"github.com/toledompm/top/internal/domain"
)

// validateRun is the strict validate phase: input-shaped invariants plus
// dependsOn resolution against this run's own spec set, then acyclicity.
// Errors carry file:line when RunInput.Sources knows the spec (contract:
// fail fast with the offending file/field).
func validateRun(in RunInput) error {
	// DesiredState.Validate: revision shape, identity uniqueness, source
	// shapes — the invariants the plan engine itself relies on.
	if err := (domain.DesiredState{Revision: in.Revision, Services: in.Specs}).Validate(); err != nil {
		return err
	}
	if err := resolveDependsOn(in); err != nil {
		return err
	}
	// Cycle detection rides on the ordering pass (same graph).
	_, err := order(in.Specs)
	return err
}

// resolveDependsOn checks every dependsOn target exists in this run's
// spec set (the spec parser already enforced intra-namespace refs).
func resolveDependsOn(in RunInput) error {
	present := make(map[domain.ServiceRef]bool, len(in.Specs))
	for _, svc := range in.Specs {
		present[svc.Ref] = true
	}
	var errs []error
	for _, svc := range in.Specs {
		for _, dep := range svc.DependsOn {
			if !present[dep] {
				field := "spec.dependsOn." + dep.String()
				if dep.Namespace != svc.Ref.Namespace {
					field = "spec.dependsOn." + dep.String() + " (cross-namespace)"
				}
				errs = append(errs, specError(in, svc.Ref, field,
					fmt.Errorf("depends on %s, which this run does not define", dep)))
			}
		}
	}
	return errors.Join(errs...)
}

// specError builds a domain.ValidationError positioned at the service's
// spec file/line when the source map carries it, naming the offending
// field.
func specError(in RunInput, ref domain.ServiceRef, field string, err error) error {
	ve := domain.ValidationError{Field: field, Reason: err.Error()}
	if pos, ok := in.Sources[ref]; ok {
		ve.File = pos.String()
	}
	return ve
}

// node is one ordering candidate in Kahn's algorithm.
type node struct {
	ref     domain.ServiceRef
	tier    int
	blocked int // unresolved dependsOn entries remaining
}

// order produces the execution order of a run: namespace tier (core →
// observability → rest, ADR-0004), then topological order over dependsOn
// (guaranteed intra-namespace by the spec parser), tie-broken by
// (namespace, name) for determinism. A dependency cycle is a
// domain.PlanConflictError naming a service still blocked by it.
func order(specs []domain.Service) ([]domain.ServiceRef, error) {
	byRef := make(map[domain.ServiceRef]bool, len(specs))
	for _, svc := range specs {
		byRef[svc.Ref] = true
	}
	nodes := make([]node, 0, len(specs))
	waiting := make(map[domain.ServiceRef][]domain.ServiceRef) // dep -> dependents
	for _, svc := range specs {
		n := node{ref: svc.Ref, tier: svc.Ref.Namespace.Tier()}
		for _, dep := range svc.DependsOn {
			if !byRef[dep] {
				continue // already a validate-phase error
			}
			n.blocked++
			waiting[dep] = append(waiting[dep], svc.Ref)
		}
		nodes = append(nodes, n)
	}
	sortNodes(nodes)

	ready := make([]int, 0, len(nodes))
	for i, n := range nodes {
		if n.blocked == 0 {
			ready = append(ready, i)
		}
	}

	ordered := make([]domain.ServiceRef, 0, len(specs))
	for len(ready) > 0 {
		ready = sortReady(ready, nodes)
		i := ready[0]
		ready = ready[1:]
		ordered = append(ordered, nodes[i].ref)
		for _, dref := range waiting[nodes[i].ref] {
			for j := range nodes {
				if nodes[j].ref.Equal(dref) {
					nodes[j].blocked--
					if nodes[j].blocked == 0 {
						ready = append(ready, j)
					}
					break
				}
			}
		}
	}
	if len(ordered) != len(specs) {
		var stuck domain.ServiceRef
		for _, n := range nodes {
			if n.blocked > 0 {
				stuck = n.ref
				break
			}
		}
		return nil, domain.PlanConflictError{
			Ref:    stuck,
			Reason: "dependency cycle in dependsOn",
			Hint:   "inspect the spec set's dependsOn fields (ADR-0004)",
		}
	}
	return ordered, nil
}

// sortReady keeps the ready queue in deterministic node order.
func sortReady(ready []int, nodes []node) []int {
	sort.SliceStable(ready, func(a, b int) bool { return nodeLess(nodes[ready[a]], nodes[ready[b]]) })
	return ready
}

// nodeLess is the deterministic node order: tier, then namespace, then name.
func nodeLess(a, b node) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	if a.ref.Namespace != b.ref.Namespace {
		return a.ref.Namespace < b.ref.Namespace
	}
	return a.ref.Name < b.ref.Name
}

func sortNodes(ns []node) {
	sort.SliceStable(ns, func(i, j int) bool { return nodeLess(ns[i], ns[j]) })
}
