// SPDX-License-Identifier: MIT

package reconcile

import (
	"github.com/toledompm/top/internal/domain"
)

// buildDiffReport assembles the T-309 report from the executed plan: one
// row per service plan (create | update | delete | noop | drifted(self))
// plus the orphaned list, each carrying the spec source when the run
// input's source map knows it.
func buildDiffReport(pl domain.Plan, in RunInput) DiffReport {
	rep := DiffReport{Revision: pl.Revision, Orphaned: pl.Orphaned}
	for _, sp := range pl.Services {
		row := ServiceDiff{Ref: sp.Ref, Kind: sp.Kind}
		if in.Prune && sp.Kind == domain.PlanDelete {
			// Delete rows carry no field changes; keep the kind (the
			// report explains the consent pair next to it).
			row.Changes = nil
		} else {
			row.Changes = sp.Changes
		}
		if pos, ok := in.Sources[sp.Ref]; ok {
			row.Source = pos
		}
		rep.Services = append(rep.Services, row)
	}
	return rep
}
