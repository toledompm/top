// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// T-402 tier-1: the adoption-aware controller-self special case over the
// fake harness. Fixture values are fictional; no git, no dokku, no sleep.

// selfSpec is a controller spec whose image matches the bootstrap
// deployment's observed state by default (the anti-redeploy posture).
func selfSpec(image string, extraEnv map[string]string) domain.Service {
	env := map[string]string{"TZ": "UTC"}
	for k, v := range extraEnv {
		env[k] = v
	}
	return domain.Service{
		Ref:     SelfRef,
		Source:  domain.Source{Image: "ghcr.io/example/top/topctl", Tag: "bootstrap-pinned"},
		Env:     env,
		Ports:   []domain.Port{{Name: "http", ContainerPort: 8443}},
		Routing: domain.Routing{Mode: domain.RoutingInternal},
	}
}

// Actually build the spec through the real constructor path (Source.Image
// + Tag compose the reference). Reference() is what plan compares.
func selfImageRef() string {
	return selfSpec("", nil).Source.Reference()
}

// bootstrapObserved is the observed state the bootstrap deploy leaves:
// the app exists, running the pinned image, with the bootstrap env keys.
func bootstrapObserved(ref domain.ServiceRef, image string) domain.ObservedState {
	o := domain.ObservedState{
		Ref:      ref,
		AppName:  mustAppName(ref),
		Deployed: true,
		Running:  true,
		Image:    image,
		EnvKeys:  []string{"TZ"},
		Ports:    []domain.Port{{Name: "http", ContainerPort: 8443}},
		Networks: []string{"top-internal"},
	}
	return o
}

// seedSelfLedger inserts the controller app's ownership row in the given
// state (the reconciler reads, never mutates, the handoff — that is the
// installer's job).
func seedSelfLedger(t *testing.T, fs *fakeStore, state store.ResourceState) {
	t.Helper()
	err := fs.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind:        inventory.KindApp,
		Identifier:  SelfAppName(),
		Labels:      map[string]string{inventory.LabelManaged: inventory.ManagedTrue, inventory.LabelNamespace: "core", inventory.LabelService: "top-controller"},
		CreatedByOp: "op_fixture",
		State:       state,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// runSelf is a one-spec, one-observed Run helper over the fakes.
func runSelf(t *testing.T, fs *fakeStore, fb *fakeBackend, svc domain.Service, obs domain.ObservedState) (RunResult, error) {
	t.Helper()
	cc := newControlledClock()
	r, err := New(Options{Backend: fb, Store: fs, Clock: cc})
	if err != nil {
		t.Fatal(err)
	}
	return r.Run(context.Background(), RunInput{
		Specs:    []domain.Service{svc},
		Revision: "ac00000000000000000000000000000000000000",
		Actor:    domain.ActorUser,
	})
}

// TestAdoptSelfNoRedeployWhenSpecMatches: the anti-redeploy invariant.
// Ledger managed (handoff recorded), the spec matches the bootstrap
// deployment → the self outcome is noop and the fake backend received
// ZERO mutating verbs for the controller app.
func TestAdoptSelfNoRedeployWhenSpecMatches(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fb := newFakeBackend()
	seedSelfLedger(t, fs, store.ResourceManaged)

	img := selfImageRef()
	fb.seed(SelfRef, func(a *fakeApp) {
		a.deployed, a.running = true, true
		a.image = img
		a.env["TZ"] = "UTC"
		a.networks = []string{"top-internal"}
		a.ports = []domain.Port{{Name: "http", ContainerPort: 8443}}
	})
	svc := selfSpec("", nil)

	res, err := runSelf(t, fs, fb, svc, bootstrapObserved(SelfRef, img))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	selfOutcome(t, res, domain.OutcomeNoop, "")
	if res.SelfAdopted != true {
		t.Errorf("SelfAdopted = false, want true (managed row on record)")
	}
	if seq := fb.sequenceOf(); len(seq) != 0 {
		t.Errorf("adoption must issue zero mutating verbs against the controller: %v", seq)
	}
}

// TestSelfPreAdoptionReportOnly: row still bootstrap-owned (or missing)
// → drift on self is the report-only skip; no apply path fires.
func TestSelfPreAdoptionReportOnly(t *testing.T) {
	for _, state := range []store.ResourceState{store.ResourceBootstrapOwned, store.ResourceState("none")} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			fs := newFakeStore()
			if state != "none" {
				seedSelfLedger(t, fs, state)
			}
			fb := newFakeBackend()
			img := selfImageRef()
			fb.seed(SelfRef, func(a *fakeApp) {
				a.deployed, a.running = true, true
				a.image = img
				a.env["TZ"] = "UTC"
				a.networks = []string{"top-internal"}
			})
			// Env drift (missing PORT key in the spec's desired env).
			svc := selfSpec("", map[string]string{"PORT": "8443"})
			svc.Ports = nil // ports convered in the adopted test

			res, err := runSelf(t, fs, fb, svc, bootstrapObserved(SelfRef, img))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			out := selfOutcome(t, res, domain.OutcomeSkipped, domain.SkipSelfDrift)
			if !strings.Contains(out.Hint, "report-only until the bootstrap handoff") {
				t.Errorf("hint = %q, want the pre-adoption posture", out.Hint)
			}
			if seq := fb.sequenceOf(); len(seq) != 0 {
				t.Errorf("pre-adoption run must not apply: %v", seq)
			}
			if res.SelfAdopted {
				t.Errorf("SelfAdopted must be false")
			}
		})
	}
}

// TestAdoptSelfImageDriftHumanGated: managed row + image/revision drift
// → skipped self-drift with the `topctl deploy` hint; no deploy verb, no
// field apply either (the whole drift bundle is the human report).
func TestAdoptSelfImageDriftHumanGated(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fb := newFakeBackend()
	seedSelfLedger(t, fs, store.ResourceManaged)

	// Bootstrap image != desired image (tag bumped in the spec).
	fb.seed(SelfRef, func(a *fakeApp) {
		a.deployed, a.running = true, true
		a.image = "ghcr.io/toledompm/top/topctl:v0.1.0"
		a.env["TZ"] = "UTC"
	})
	svc := selfSpec("", nil)
	svc.Source.Tag = "v2" // new controller revision in git

	res, err := runSelf(t, fs, fb, svc, bootstrapObserved(SelfRef, "ghcr.io/toledompm/top/topctl:v0.1.0"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := selfOutcome(t, res, domain.OutcomeSkipped, domain.SkipSelfDrift)
	if !strings.Contains(out.Hint, "topctl deploy core/top-controller") ||
		!strings.Contains(out.Hint, "never applies") {
		t.Errorf("hint = %q, want the ADR-0005 human-gate wording", out.Hint)
	}
	if seq := fb.sequenceOf(); len(seq) != 0 {
		t.Errorf("image-drift self must never redeploy nor set fields: %v", seq)
	}
}

// TestAdoptSelfFieldDriftConverges: managed row + env-only drift → the
// normal per-service path: env set, no deploy verb (the drift carries no
// revision entry, so DeployImage never runs for the controller).
func TestAdoptSelfFieldDriftConverges(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	fb := newFakeBackend()
	seedSelfLedger(t, fs, store.ResourceManaged)

	img := selfImageRef()
	fb.seed(SelfRef, func(a *fakeApp) {
		a.deployed, a.running = true, true
		a.image = img
		a.env["TZ"] = "UTC"
		a.networks = []string{"top-internal"}
		a.ports = []domain.Port{{Name: "http", ContainerPort: 8443}}
	})
	svc := selfSpec("", map[string]string{"LOG_LEVEL": "info"})

	res, err := runSelf(t, fs, fb, svc, bootstrapObserved(SelfRef, img))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out := selfOutcome(t, res, domain.OutcomeSucceeded, "")
	if !slices.Contains(out.Changes, "+env.LOG_LEVEL: <absent> → info") {
		t.Errorf("changes = %v, want the env add entry", out.Changes)
	}
	// The ledger row that exists is left alone (ledgerApp is idempotent);
	// the only mutating verbs must be EnsureApp (adopt-if-exists) + the
	// env set — never a deploy.
	seq := fb.sequenceOf()
	if len(seq) != 2 || !strings.HasPrefix(seq[0], "ensure:") ||
		seq[1] != "env:core/top-controller:2" {
		t.Errorf("sequence = %v, want [ensure:… env:core/top-controller:2]", seq)
	}
	for _, s := range seq {
		if strings.HasPrefix(s, "deploy:") {
			t.Errorf("adoped self must never reach DeployImage: %v", seq)
		}
	}
}

// selfOutcome finds the controller-self outcome and asserts its shape.
func selfOutcome(t *testing.T, res RunResult, want domain.OutcomeResult, skip domain.SkipReason) domain.ServiceOutcome {
	t.Helper()
	for _, o := range res.Outcomes {
		if o.Ref.Equal(SelfRef) {
			if o.Result != want || o.Skip != skip {
				t.Fatalf("self outcome = %+v, want result %s skip %q", o, want, skip)
			}
			return o
		}
	}
	t.Fatalf("no self outcome in %+v", res.Outcomes)
	return domain.ServiceOutcome{}
}
