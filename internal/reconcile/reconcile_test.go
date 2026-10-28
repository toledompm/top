// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// Fixtures --------------------------------------------------------------

const testRevision = "a9b8c7d6e5f4a3b2c1d0a9b8c7d6e5f4a3b2c1d0"

const testImageDigest = "docker.io/prom/prometheus@sha256:" +
	"3f1a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6a7b8c9d0e1f2a3b4c"

func testImage() domain.Source {
	return domain.Source{Image: "docker.io/prom/prometheus", Digest: "sha256:3f1a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f6a7b8c9d0e1f2a3b4c"}
}

func buildSpec(ref domain.ServiceRef, shape func(*domain.Service), deps ...domain.ServiceRef) domain.Service {
	svc := domain.Service{Ref: ref, Source: testImage()}
	svc.DependsOn = append(svc.DependsOn, deps...)
	if shape != nil {
		shape(&svc)
	}
	return svc
}

type harness struct {
	r       *Reconciler
	backend *fakeBackend
	store   *fakeStore
	clock   *controlledClock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fb := newFakeBackend()
	fs := newFakeStore()
	cc := newControlledClock()
	r, err := New(Options{
		Backend:    fb,
		Store:      fs,
		Clock:      cc,
		Secrets:    fakeSecrets{values: map[string]string{"SMTP_PASSWORD": "s3cr3t"}},
		VolumeRoot: "/var/lib/top/volumes-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &harness{r: r, backend: fb, store: fs, clock: cc}
}

func (h *harness) run(t *testing.T, in RunInput) (RunResult, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return h.r.Run(ctx, in)
}

// --- tests ---------------------------------------------------------------

func happyInput() RunInput {
	return RunInput{
		Specs: []domain.Service{
			buildSpec(domain.Ref(domain.ObservabilityNamespace, "prometheus"), nil),
			buildSpec(domain.Ref(domain.DefaultNamespace, "grafana"), func(s *domain.Service) {
				s.Env = map[string]string{"GF_SERVER_HTTP_PORT": "3000"}
			}, domain.Ref(domain.DefaultNamespace, "db")),
			buildSpec(domain.Ref(domain.DefaultNamespace, "db"), nil),
			SelfRefHoldout(),
		},
		Revision:      testRevision,
		Actor:         domain.ActorUser,
		CorrelationID: "test-run",
		HolderID:      "test-holder",
	}
}

func SelfRefHoldout() domain.Service {
	return buildSpec(SelfRef, nil)
}

func TestHappyPathOrderOutcomeAndRecord(t *testing.T) {
	h := newHarness(t)
	res, err := h.run(t, happyInput())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != domain.RunSucceeded {
		for _, o := range res.Outcomes {
			t.Logf("outcome %v: %v %s %s", o.Ref, o.Result, o.Err, o.Hint)
		}
		t.Fatalf("status %s, want succeeded", res.Status)
	}

	// Order: core tier first, then observability, then default topo
	// (db before grafana, its dependent).
	var order []string
	for _, o := range res.Outcomes {
		order = append(order, o.Ref.String())
	}
	want := []string{"core/top-controller", "observability/prometheus", "default/db", "default/grafana"}
	// Order is tier-sorted; core/top-controller is self → outcome noop, kept in place.
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("outcome order %v, want %v", order, want)
	}

	// Deployment calls logged per spec (image reference passed through).
	seq := h.backend.sequenceOf()
	if len(seq) < 3 {
		t.Fatalf("too few backend mutations: %v", seq)
	}
	var deploys int
	for _, s := range seq {
		if strings.HasPrefix(s, "deploy:") {
			deploys++
			if !strings.Contains(s, testImageDigest) {
				t.Errorf("deploy image ref %q, want %q", s, testImageDigest)
			}
		}
	}
	if deploys != 3 { // db, grafana, prometheus — NOT the self service
		t.Errorf("deploy calls %d, want 3 (controller-self must not deploy): seq %v", deploys, seq)
	}

	// Operation history exists and is terminal.
	for _, op := range h.store.ops {
		if !op.IsTerminal() {
			t.Errorf("operation %s not terminal", op.ID)
		}
	}
	if !h.store.released {
		t.Error("lease not released after the run")
	}
	if len(h.store.runs) != 1 {
		t.Fatalf("runs recorded %d, want 1", len(h.store.runs))
	}
	run := h.store.runs[0]
	if run.Status != domain.RunSucceeded || run.Actor != domain.ActorUser {
		t.Errorf("run record %+v", run)
	}
}

func TestSecondRunIdempotentZeroMutations(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, happyInput()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := len(h.backend.sequenceOf())

	res, err := h.run(t, happyInput())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Status != domain.RunSucceeded {
		t.Fatalf("second run status %s", res.Status)
	}
	for _, sp := range res.Plan.Services {
		if sp.Ref.Equal(SelfRef) {
			continue // controller-self drift is a standing report (never applied)
		}
		if !sp.Empty() {
			t.Fatalf("second run plan for %s not empty: %s", sp.Ref, sp.String())
		}
	}
	after := len(h.backend.sequenceOf())
	if adds := after - before; adds != 0 {
		t.Errorf("second run mutated the host %d time(s) (idempotency invariant): %v",
			adds, h.backend.sequenceOf()[before:])
	}
	for _, o := range res.Outcomes {
		if o.Result != domain.OutcomeNoop && o.Result != domain.OutcomeSkipped {
			t.Errorf("second run outcome %v: %s, want noop/skipped", o.Ref, o.Result)
		}
	}
}

func TestApplyFailureContainedDependantsSkipIndependentContinue(t *testing.T) {
	h := newHarness(t)
	db := h.backend.seed(domain.Ref(domain.DefaultNamespace, "db"), func(a *fakeApp) { a.failDeploy = errors.New("boom: image pull refused") })
	_ = db

	in := happyInput()
	in.Specs = append(in.Specs, buildSpec(domain.Ref(domain.DefaultNamespace, "independent"), nil))
	res, err := h.run(t, in)
	if err == nil {
		t.Fatal("failed apply must surface a run-level error (contract: run failed)")
	}
	if res.Status != domain.RunFailed {
		t.Fatalf("status %s", res.Status)
	}
	if !errors.Is(err, domain.ErrConflict) && !strings.Contains(err.Error(), "boom") {
		t.Fatalf("run error %v does not carry the contained failure", err)
	}

	byRef := map[domain.ServiceRef]domain.ServiceOutcome{}
	for _, o := range res.Outcomes {
		byRef[o.Ref] = o
	}
	dbOut := byRef[domain.Ref(domain.DefaultNamespace, "db")]
	if dbOut.Result != domain.OutcomeFailed || !strings.Contains(dbOut.Err, "boom") {
		t.Errorf("db outcome %+v, want failed with the cause", dbOut)
	}
	if dbOut.Hint == "" {
		t.Error("failed outcome must carry the follow-up hint")
	}
	gOut := byRef[domain.Ref(domain.DefaultNamespace, "grafana")]
	if gOut.Result != domain.OutcomeSkipped || gOut.Skip != domain.SkipDependencyFailed {
		t.Errorf("grafana outcome %+v, want skipped/dependency-failed", gOut)
	}
	inOut := byRef[domain.Ref(domain.DefaultNamespace, "independent")]
	if inOut.Result != domain.OutcomeSucceeded {
		t.Errorf("independent outcome %+v, want succeeded (independent services continue)", inOut)
	}
	// Last-known-good per service: the failed service's known-good must
	// NOT advance; the independent one must.
	cp := h.store.lastCheckpoint()
	if _, ok := cp.LastKnownGoodPerService[domain.Ref(domain.DefaultNamespace, "db")]; ok {
		t.Error("failed service advanced last-known-good")
	}
	if _, ok := cp.LastKnownGoodPerService[domain.Ref(domain.DefaultNamespace, "independent")]; !ok {
		t.Error("green service missing from last-known-good")
	}
	if cp.LastKnownGood != "" {
		t.Error("partial failure must not claim repo-wide last-known-good")
	}
}

func TestReadinessDeadlineFailsContained(t *testing.T) {
	h := newHarness(t)
	prom := h.backend.seed(domain.Ref(domain.ObservabilityNamespace, "prometheus"), func(a *fakeApp) {
		a.readinessHold = 5 // refuses to report running for 5 polls
	})
	_ = prom

	in := RunInput{
		Specs:    []domain.Service{buildSpec(domain.Ref(domain.ObservabilityNamespace, "prometheus"), nil)},
		Revision: testRevision,
		Actor:    domain.ActorUser,
	}
	// Override pacing: deadline 20s, poll 10s → the gate fails within ~3 polls.
	r, err := New(Options{
		Backend: h.backend, Store: h.store, Clock: h.clock,
		ReadinessDeadline: 20 * time.Second, ReadinessPoll: 10 * time.Second,
		VolumeRoot: "/var/lib/top/volumes-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.r = r
	res, err := h.run(t, in)
	if err == nil {
		t.Fatal("readiness miss must fail the run")
	}
	for _, o := range res.Outcomes {
		if o.Result != domain.OutcomeFailed || !strings.Contains(o.Err, "readiness") {
			t.Errorf("outcome %+v, want failed readiness text", o)
		}
	}
}

func TestLeaseConflictBlocksSecondRun(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, happyInput()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A second holder now owns the lease: the fake reports ErrLocked.
	before := len(h.backend.sequenceOf())
	h.store.leaseErr = domain.LockedError{Lease: LeaseName, Holder: "ops-offline-recovery"}
	res, err := h.run(t, happyInput())
	if !IsErrLockedLease(err) {
		t.Fatalf("error %v, want LeaseConflictError", err)
	}
	var lce LeaseConflictError
	if !errors.As(err, &lce) || lce.Lease != LeaseName || lce.Holder != "ops-offline-recovery" {
		t.Fatalf("typed lease error %+v", err)
	}
	if !errors.Is(err, domain.ErrLocked) {
		t.Fatal("LeaseConflictError must unwrap to domain.ErrLocked")
	}
	if !res.Plan.Empty() && len(res.Outcomes) != 0 {
		t.Errorf("a blocked run must not produce outcomes: %+v", res.Outcomes)
	}
	if after := len(h.backend.sequenceOf()); after != before {
		t.Errorf("blocked run applied mutations: %v", h.backend.sequenceOf()[before:])
	}
	// The store released the first run's lease on completion and the
	// second run's acquire returned without writing an operation record.
	if opn := len(h.store.ops); opn != 1 {
		t.Errorf("operations recorded %d, want 1 (blocked run never started)", opn)
	}
}

func TestPruneConsentPairRequired(t *testing.T) {
	h := newHarness(t)
	in := happyInput()
	in.Prune = true
	in.Confirm = false // missing the confirmation half
	_, err := h.run(t, in)
	if err == nil || !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("error %v, want ErrInvalid consent-pair refusal", err)
	}
	if len(h.store.runs) != 0 {
		t.Errorf("consent refusal happened before the run: recorded %d runs", len(h.store.runs))
	}
}

func TestPruneDeletesOnlyThroughTheTwoKeyGate(t *testing.T) {
	h := newHarness(t)
	legacy := domain.Ref(domain.Namespace("apps"), "legacy")
	gate := &fakePruneGate{allowed: map[domain.ServiceRef]bool{legacy: true}}
	h.r.pruneGate = gate
	// seed host/ledger parity; the fake gate says yes regardless.
	if err := h.store.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind: "app", Identifier: "apps-legacy", State: store.ResourceManaged,
	}); err != nil {
		t.Fatal(err)
	}

	in := happyInput()
	in.Prune = true
	in.Confirm = true
	in.Namespaces = []string{"core", "observability", "default", "apps"}
	res, err := h.run(t, in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if found := containsRef(res.Orphaned, legacy); !found {
		t.Fatalf("orphan %s not reported: %v", legacy, res.Orphaned)
	}
	if !containsRef(res.Pruned, legacy) {
		t.Fatalf("pruned %v, want legacy", res.Pruned)
	}
	if !h.backend.destroyCalled(legacy) {
		t.Error("backend.Destroy not called for the allowed candidate")
	}
	if len(gate.asked) != 1 || !gate.asked[0].Equal(legacy) {
		t.Errorf("gate consulted %v", gate.asked)
	}
	if _, err := h.store.ListLedger(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	for keys := range h.store.ledger {
		if keys == "app|apps-legacy" {
			t.Error("ledger row not dropped after the deletion")
		}
	}
}

func TestPruneDeletionRefusalPassThrough(t *testing.T) {
	h := newHarness(t)
	legacy := domain.Ref(domain.Namespace("apps"), "legacy")
	gate := &fakePruneGate{} // refuses everything with its reason
	h.r.pruneGate = gate
	if err := h.store.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind: "app", Identifier: "apps-legacy", State: store.ResourcePreExisting,
	}); err != nil {
		t.Fatal(err)
	}
	in := happyInput()
	in.Prune = true
	in.Confirm = true
	in.Namespaces = []string{"core", "observability", "default", "apps"}
	res, err := h.run(t, in)
	if err != nil {
		t.Fatalf("a refusal is a report, not a run failure: %v", err)
	}
	if h.backend.destroyCalled(legacy) {
		t.Fatal("a refused candidate must never reach Destroy")
	}
	if len(res.PruneRefusals) != 1 || res.PruneRefusals[0].Ref.String() != "apps/legacy" {
		t.Fatalf("refusals %+v", res.PruneRefusals)
	}
	if !strings.Contains(res.PruneRefusals[0].Reason, "refused-by-fake-gate") {
		t.Errorf("refusal reason %q must carry the gate's own reason", res.PruneRefusals[0].Reason)
	}
}

func TestLedgerPruneGateDelegatesToInventoryTwoKeyRule(t *testing.T) {
	fs := newFakeStore()
	managed := "apps-legacy"
	if err := fs.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind: "app", Identifier: managed, State: store.ResourceManaged,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind: "app", Identifier: "default-ghost", State: store.ResourcePreExisting,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fs.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind: "app", Identifier: "default-vanished", State: store.ResourceManaged,
	}); err != nil {
		t.Fatal(err)
	}
	gate := LedgerPruneGate{
		St:         fs,
		Namespaces: []string{"core", "observability", "default", "apps"},
		Snap: func(ctx context.Context) (*inventory.HostInventory, error) {
			return &inventory.HostInventory{DokkuApps: []string{managed, "default-ghost"}}, nil
		},
	}

	// Key OK: ledger owned + snapshot present + naming agrees.
	ok, reason, err := gate.CanDelete(context.Background(), domain.Ref(domain.Namespace("apps"), "legacy"))
	if err != nil || !ok {
		t.Fatalf("managed+present candidate: allowed=%v reason=%q err=%v", ok, reason, err)
	}
	// Ledger says pre-existing: refuse with the inventory reason.
	ok, reason, _ = gate.CanDelete(context.Background(), domain.Ref(domain.DefaultNamespace, "ghost"))
	if ok || !strings.Contains(reason, inventory.ReasonRefusedPreExisting.String()) {
		t.Errorf("pre-existing candidate: allowed=%v reason=%q", ok, reason)
	}
	// Snapshot lacks the app: refusal (second key of the two-key rule).
	ok, reason, _ = gate.CanDelete(context.Background(), domain.Ref(domain.DefaultNamespace, "vanished"))
	if ok || !strings.Contains(reason, inventory.ReasonAbsentSnapshot.String()) {
		t.Errorf("snapshot-absent candidate: allowed=%v reason=%q", ok, reason)
	}
}

func TestValidationErrorsCarrySpecSourcesAndStopBeforeMutations(t *testing.T) {
	h := newHarness(t)
	in := happyInput()
	// deps reference a service this run does not define.
	in.Specs = append(in.Specs[:0], in.Specs[:3]...)
	in.Specs[1].DependsOn = []domain.ServiceRef{domain.Ref(domain.DefaultNamespace, "missing")}
	in.Sources = map[domain.ServiceRef]SourcePos{
		domain.Ref(domain.DefaultNamespace, "grafana"): {File: "default/grafana.yaml", Line: 3},
	}
	before := len(h.backend.sequenceOf())
	res, err := h.run(t, in)
	if err == nil {
		t.Fatal("validation failure must fail the run")
	}
	var ve domain.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v, want domain.ValidationError", err)
	}
	if !strings.Contains(ve.Error(), "default/grafana.yaml:3") {
		t.Errorf("validation error %q must carry file:line", ve.Error())
	}
	if len(res.Outcomes) != 0 {
		t.Errorf("validate-phase failures carry no outcomes: %+v", res.Outcomes)
	}
	if added := len(h.backend.sequenceOf()) - before; added != 0 {
		t.Errorf("validation failure mutated the host: %v", h.backend.sequenceOf())
	}
	// Recorded: failed run, per-file error text, no lease.
	if len(h.store.runs) != 1 || h.store.runs[0].Status != domain.RunFailed {
		t.Errorf("runs %+v", h.store.runs)
	}
	if h.store.runs[0].ValidationErr == "" {
		t.Error("run record must carry the validation error")
	}
}

func TestControllerSelfServiceReportOnly(t *testing.T) {
	h := newHarness(t)
	in := happyInput()
	// Drift on controller-self: the spec asks for an env key the host lacks.
	for i, s := range in.Specs {
		if s.Ref.Equal(SelfRef) {
			in.Specs[i].Env = map[string]string{"TOP_SELF_EXTRA": "1"}
		}
	}
	res, err := h.run(t, in)
	if err != nil {
		t.Fatalf("self drift is reported, not failed: %v", err)
	}
	if res.Status != domain.RunSucceeded {
		t.Fatalf("status %s: self-report must not fail the run", res.Status)
	}
	var self domain.ServiceOutcome
	found := false
	for _, o := range res.Outcomes {
		if o.Ref.Equal(SelfRef) {
			self, found = o, true
		} else {
			o := o
			if o.Result == domain.OutcomeSkipped {
				t.Errorf("unexpected other skip %+v", o)
			}
		}
	}
	if !found {
		t.Fatal("controller-self outcome missing")
	}
	if self.Result != domain.OutcomeSkipped || self.Skip != domain.SkipSelfDrift {
		t.Fatalf("self outcome %+v, want skipped/self-drift-reported", self)
	}
	if self.Changes == nil {
		t.Error("the report must carry the drift changes")
	}
	// Nothing was applied to the controller app.
	if a, ok := h.backend.apps["core-top-controller"]; ok && len(a.env) > 0 {
		t.Errorf("controller-self was mutated: env %v", a.env)
	}
}

func TestSourceBuildRevisionChangeFailsThisWave(t *testing.T) {
	h := newHarness(t)
	builder := domain.Ref(domain.DefaultNamespace, "builder")
	in := RunInput{
		Specs: []domain.Service{buildSpec(builder, func(s *domain.Service) {
			s.Source = domain.Source{Build: &domain.BuildSource{
				Repo: "github.com/example/apps", Ref: testRevision,
			}}
		})},
		Revision: testRevision,
		Actor:    domain.ActorUser,
	}
	// The host already runs the OLD revision; the desired sha differs so
	// the plan wants a revision change — which the v0 seam cannot apply.
	h.backend.seed(builder, func(a *fakeApp) {
		a.deployed = true
		a.running = true
		a.revision = strings.Repeat("1", 40)
	})
	res, err := h.run(t, in)
	if res.Status != domain.RunFailed {
		t.Fatalf("status %s, want failed (contained)", res.Status)
	}
	for _, o := range res.Outcomes {
		if !strings.Contains(o.Err, "git:sync") {
			t.Errorf("outcome %+v must name the missing seam", o)
		}
	}
	if vErr := err; err != nil && !strings.Contains(err.Error(), "git:sync") {
		t.Logf("run error: %v", vErr)
	}
}

func TestNoopOutcomeRecordedForConvergedService(t *testing.T) {
	h := newHarness(t)
	ref := domain.Ref(domain.DefaultNamespace, "settled")
	h.backend.seed(ref, func(a *fakeApp) {
		a.deployed = true
		a.running = true
		a.image = testImageDigest
	})
	in := RunInput{
		Specs:    []domain.Service{buildSpec(ref, nil)},
		Revision: testRevision,
		Actor:    domain.ActorUser,
	}
	res, err := h.run(t, in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Outcomes) != 1 || res.Outcomes[0].Result != domain.OutcomeNoop {
		t.Fatalf("outcomes %+v, want one noop", res.Outcomes)
	}
}

func containsRef(refs []domain.ServiceRef, want domain.ServiceRef) bool {
	for _, r := range refs {
		if r.Equal(want) {
			return true
		}
	}
	return false
}
