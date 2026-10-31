// SPDX-License-Identifier: MIT

package controller

import (
	"testing"

	"github.com/toledompm/top/internal/obs"
)

// countMetric sums the value of every sample of `name` whose label
// `labelName` equals `labelValue`, gathered from m's private registry.
// (testutil is avoided deliberately: gathering is five lines and keeps
// the import surface exactly what obs already uses.)
func countMetric(t *testing.T, m *obs.Metrics, name, labelName, labelValue string) float64 {
	t.Helper()
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		sum := 0.0
		for _, s := range mf.GetMetric() {
			for _, l := range s.GetLabel() {
				if l.GetName() == labelName && l.GetValue() == labelValue {
					sum += s.GetCounter().GetValue()
					break
				}
			}
		}
		return sum
	}
	t.Fatalf("metric family %q not found", name)
	return 0
}
