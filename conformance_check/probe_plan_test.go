//go:build conformance

package conformance_check

import (
	"testing"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/plan"
	"github.com/toledompm/top/internal/spec"
)

// loadFixtures is conformance's own copy of the fixture loader (internal/
// plan's mustLoadFixtures is in its _test.go and cannot be imported; the
// synthesizer seam was extracted as plan.SynthesizeObserved instead — see
// internal/plan/synthesize.go).
func loadFixtures(t *testing.T) []domain.Service {
	t.Helper()
	res, err := spec.LoadDir("../internal/spec/testdata/valid", spec.WithPlatformRepo(true))
	if err != nil {
		t.Fatalf("spec.LoadDir fixtures: %v", err)
	}
	if len(res.Services) == 0 {
		t.Fatal("fixture dir produced no services")
	}
	return res.Services
}

// TestLoadDirProducesDomainVocabulary pins service-spec.md → internal/spec →
// internal/domain: parsing the contract's illustrative fixtures yields
// resolved, validated domain.Services — the vocabulary every other package
// consumes.
func TestLoadDirProducesDomainVocabulary(t *testing.T) {
	services := loadFixtures(t)
	if len(services) < 3 {
		t.Fatalf("got %d services, want the three contract namespaces covered", len(services))
	}
	for _, s := range services {
		if s.Ref.IsZero() {
			t.Fatalf("fixture %s: zero identity", s.Source.Reference())
		}
		if _, err := domain.AppName(s.Ref.Namespace, s.Ref.Name); err != nil {
			t.Fatalf("fixture %s: AppName: %v", s.Ref, err)
		}
	}
}

// TestSpecPlanNoopIdempotency pins the reconciler contract's invariant at
// the contract boundary ("Idempotency: applying the same desired state twice
// changes nothing and returns the same plan") and plan/README.md's own
// statement, through real spec-parsed fixtures, real plan math, and an
// observed state synthesized against the platform-backend Inspect table —
// not against Compute's internals (plan.SynthesizeObserved).
func TestSpecPlanNoopIdempotency(t *testing.T) {
	services := loadFixtures(t)
	for _, d := range services {
		sp := plan.Compute(d, plan.SynthesizeObserved(d))
		if sp.Kind != domain.PlanNoop || len(sp.Changes) != 0 {
			t.Fatalf("%s: Compute(converged) = %s with %d changes, want noop/0: %v",
				d.Ref, sp.Kind, len(sp.Changes), sp.Changes)
		}
	}

	// Fleet level: ComputeAll over synthesized observed is all-noop and
	// reports no orphans.
	obs := map[domain.ServiceRef]domain.ObservedState{}
	for _, d := range services {
		obs[d.Ref] = plan.SynthesizeObserved(d)
	}
	p := plan.ComputeAll(services, obs)
	for _, sp := range p.Services {
		if sp.Kind != domain.PlanNoop || len(sp.Changes) != 0 {
			t.Fatalf("%s: ComputeAll(converged) = %s with %d changes",
				sp.Ref, sp.Kind, len(sp.Changes))
		}
	}
	if len(p.Orphaned) != 0 {
		t.Fatalf("orphaned = %v, want none", p.Orphaned)
	}
}

// TestSelfServiceDriftIsReportOnly pins the reconciler contract's
// controller-self-service rule (ADR-0005) end to end.
func TestSelfServiceDriftIsReportOnly(t *testing.T) {
	services := loadFixtures(t)
	var self domain.Service
	for _, d := range services {
		if d.Ref.Namespace == domain.CoreNamespace {
			self = d
		}
	}
	if self.Ref.IsZero() {
		t.Fatal("no core-namespace fixture to mark as self")
	}
	// Drift on the self-ref: desired moves ahead of what the host has.
	// Synthesize the observed FIRST (converged world), then add a key.
	obs := plan.SynthesizeObserved(self)
	self.Env["NEW_KEY"] = "new-value"
	sp := plan.Compute(self, obs, plan.WithSelfService(self.Ref))
	if sp.Kind != domain.PlanDriftedSelf {
		t.Fatalf("kind = %s, want drifted(self); changes %+v", sp.Kind, sp.Changes)
	}
	if len(sp.Changes) == 0 {
		t.Fatal("drifted(self) must still carry its report of field changes")
	}
}
