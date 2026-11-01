// SPDX-License-Identifier: MIT

package install

import (
	"context"
	"errors"
	"io"
	"os"
	stdexec "os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store"
)

// T-402 phase-5 (first reconciliation + ADR-0005 handoff) tier-1 tests:
// fake-runner + temp sqlite, fictional values only.

// countedHandoff is the FirstRunner test double: a canned report (and a
// failing mode), with call counting for the no-re-run assertions.
type countedHandoff struct {
	report FirstRunReport
	err    error
	calls  int
}

func (h *countedHandoff) FirstRun(context.Context) (FirstRunReport, error) {
	h.calls++
	return h.report, h.err
}

// controllerAppRow fetches the controller app's ledger row by identity.
func controllerAppRow(t *testing.T, stx store.Store) *store.Resource {
	t.Helper()
	rows, err := stx.ListLedger(context.Background(), inventory.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Identifier == "core-top-controller" {
			return r
		}
	}
	return nil
}

// adoptOptions runs testOptions + the handoff; store is shared between
// install and the (fake) handoff.
func adoptOptions(t *testing.T, r exec.Runner, stx store.Store, f *exectest.FakeRunner, handoff FirstRunner) Options {
	t.Helper()
	opts := testOptions(t, r, stx, f)
	opts.FirstRun = handoff
	return opts
}

// groupedDeployVerbs counts the mutating verbs the anti-redeploy
// invariant cares about across the WHOLE call log.
func groupedDeployVerbs(t *testing.T, f *exectest.FakeRunner) (fromImageCtrl, rebuilds int) {
	t.Helper()
	for _, c := range f.Recorded() {
		if c.Name != "dokku" {
			continue
		}
		switch {
		case slices.Contains(c.Args, "git:from-image") && slices.Contains(c.Args, "core-top-controller"):
			fromImageCtrl++
		case slices.Contains(c.Args, "ps:rebuild"), slices.Contains(c.Args, "ps:restart"), slices.Contains(c.Args, "ps:stop"):
			rebuilds++
		}
	}
	return fromImageCtrl, rebuilds
}

// TestInstallPhase5AdoptsAndRunsFirstReconcile: the golden — phase 5 runs
// the handoff, the controller row flips bootstrap-owned → managed with
// the adopted-by-op annotation on the phase op, and NO redeploy verb ran
// (exactly one phase-4 git:from-image, zero rebuilds/restarts, and the
// handoff itself carries no host verbs by construction).
func TestInstallPhase5AdoptsAndRunsFirstReconcile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	h := &countedHandoff{report: FirstRunReport{
		Revision:       "ac00000000000000000000000000000000000000",
		RunID:          "op_fixturereconcile00000000000000000000",
		ControllerSpec: true,
		Succeeded:      true,
		Status:         "succeeded",
	}}
	opts := adoptOptions(t, r, st, f, h)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if h.calls != 1 {
		t.Errorf("handoff calls = %d, want 1", h.calls)
	}

	// The ledger flip: the WHOLE adoption's mutation.
	row := controllerAppRow(t, st)
	if row == nil || row.State != store.ResourceManaged {
		t.Fatalf("controller row after install = %+v, want managed", row)
	}
	adopted := slices.ContainsFunc(res.Adopted, func(s string) bool {
		return strings.Contains(s, "ownership core-top-controller bootstrap-owned → managed") &&
			strings.Contains(s, "adopted by op ")
	})
	if !adopted {
		t.Errorf("adopted list must record the conversion: %v", res.Adopted)
	}

	// The adopted-by-op annotation lives on the phase-5 op record
	// (details), together with the first run's id/revision.
	var p5 *domain.Operation
	for _, op := range installOps(t, st) {
		if p, ok := phaseOf(op); ok && p == phaseFirstReconcile && op.State == domain.OpSucceeded {
			p5 = op // newest-first: the succeeded phase-5 op
		}
	}
	if p5 == nil {
		t.Fatalf("phase-5 op missing")
	}
	if p5.Details["adopted"] != true || p5.Details["adoptedByOp"] != p5.ID.String() {
		t.Errorf("phase-5 details lack adoption markers: %v", p5.Details)
	}
	if p5.Details["firstRunOp"] != h.report.RunID || p5.Details["firstRunRevision"] != h.report.Revision {
		t.Errorf("phase-5 details lack the first-run anchors: %v", p5.Details)
	}

	// Anti-redeploy proof over the full call log.
	fromImage, rebuilds := groupedDeployVerbs(t, f)
	if fromImage != 1 || rebuilds != 0 {
		t.Errorf("phase-5 ran deploy verbs: git:from-image(controller)=%d rebuild/restart=%d", fromImage, rebuilds)
	}
}

// TestInstallAdoptionIdempotent: a re-run after a successful adoption
// re-adopts NOTHING — phase 5 is op-record-skipped, the ledger row stays
// managed with its original created_by anchor, and no deploy verb runs.
func TestInstallAdoptionIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	h := &countedHandoff{report: FirstRunReport{
		RunID: "op_fixture", ControllerSpec: true, Succeeded: true,
	}}
	opts := adoptOptions(t, r, st, f, h)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	if _, err := Install(ctx, opts); err != nil {
		t.Fatalf("install 1: %v", err)
	}
	before := controllerAppRow(t, st)

	// Re-run: fresh seed for phase 0/4-safe fixtures (network/write
	// verbs tolerate re-runs through Ensure* semantics).
	f2 := exectest.NewFakeRunner()
	opts2 := adoptOptions(t, f2, st, f2, h)
	seedDoctor(f2, false)
	seedRepo(f2)
	seedSnapshot(f2)
	seedPhases(f2)
	res2, err := Install(ctx, opts2)
	if err != nil {
		t.Fatalf("install 2: %v", err)
	}
	if !slices.Contains(res2.PhasesSkipped, phaseNames[phaseFirstReconcile]) {
		t.Errorf("re-run must skip phase 5: %v", res2.PhasesSkipped)
	}
	if len(res2.Adopted) != 0 {
		t.Errorf("re-run must re-adopt nothing: %v", res2.Adopted)
	}
	after := controllerAppRow(t, st)
	if after == nil || after.State != store.ResourceManaged ||
		after.CreatedByOp != before.CreatedByOp || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("ledger row changed on the idempotent re-run: before=%+v after=%+v", before, after)
	}
	for _, c := range f2.Recorded() {
		if slices.Contains(c.Args, "git:from-image") || slices.Contains(c.Args, "ps:rebuild") {
			t.Fatalf("re-run ran a deploy verb: %s", c.String())
		}
	}
}

// TestInstallAdoptionNoSpecStaysBootstrapOwned: no core/top-controller
// spec → info finding, the row STAYS bootstrap-owned, and the next
// install REDOES phase 5 (completion keyed on the adoption record) and
// adopts once the spec lands.
func TestInstallAdoptionNoSpecStaysBootstrapOwned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	h := &countedHandoff{report: FirstRunReport{
		RunID: "op_fixture", ControllerSpec: false, Succeeded: true,
	}}
	opts := adoptOptions(t, r, st, f, h)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("install (no controller spec is an info finding, not a failure): %v", err)
	}
	if row := controllerAppRow(t, st); row == nil || row.State != store.ResourceBootstrapOwned {
		t.Fatalf("controller row = %+v, want bootstrap-owned", row)
	}
	if !slices.ContainsFunc(res.Skipped, func(s string) bool {
		return strings.Contains(s, "defines no core/top-controller spec")
	}) {
		t.Errorf("skipped list must carry the finding: %v", res.Skipped)
	}

	// The spec lands later: the same repo is re-installed, phase 5
	// redoes it, and the handoff completes the handoff.
	h.report.ControllerSpec = true
	res2, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("install 2: %v", err)
	}
	if slices.Contains(res2.PhasesSkipped, phaseNames[phaseFirstReconcile]) {
		t.Errorf("phase 5 must redo after an unadopted success: %v", res2.PhasesSkipped)
	}
	if row := controllerAppRow(t, st); row == nil || row.State != store.ResourceManaged {
		t.Errorf("controller row after re-adopt = %+v, want managed", row)
	}
}

// TestInstallAdoptionFailureStops: a non-green first run fails the phase,
// the ledger stays bootstrap-owned, and the failure is the typed,
// actionable error the contract's failure behavior prescribes.
func TestInstallAdoptionFailureStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	h := &countedHandoff{report: FirstRunReport{
		RunID: "op_fixture", ControllerSpec: true, Succeeded: false, Status: "failed",
	}}
	opts := adoptOptions(t, r, st, f, h)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	res, err := Install(ctx, opts)
	if err == nil {
		t.Fatal("a non-green first reconciliation must fail the install")
	}
	if !strings.Contains(err.Error(), `"failed", not green`) {
		t.Errorf("err = %v, want the non-green, actionable text", err)
	}
	if row := controllerAppRow(t, st); row == nil || row.State != store.ResourceBootstrapOwned {
		t.Errorf("controller row = %+v, want bootstrap-owned (handoff never flips on a failed run)", row)
	}
	var p5fail bool
	for _, op := range installOps(t, st) {
		if p, ok := phaseOf(op); ok && p == phaseFirstReconcile && op.State == domain.OpFailed {
			p5fail = true
		}
	}
	if !p5fail {
		t.Errorf("the failing phase-5 op must record itself failed")
	}
	if res == nil {
		t.Errorf("a failed install still returns the partial Result")
	}
}

// TestInstallAdoptionHandoffErrorStops: a handoff that ERRORS (git/
// spec-load failure) fails the phase; nothing flipped.
func TestInstallAdoptionHandoffErrorStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	h := &countedHandoff{err: errors.New("git unreachable (fixture)")}
	opts := adoptOptions(t, r, st, f, h)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	if _, err := Install(ctx, opts); err == nil || !strings.Contains(err.Error(), "first reconciliation") {
		t.Fatalf("err = %v, want the wrapped handoff failure", err)
	}
	if row := controllerAppRow(t, st); row == nil || row.State != store.ResourceBootstrapOwned {
		t.Errorf("row = %+v, want bootstrap-owned", row)
	}
}

// TestAdoptRefusalsDirect: the ledger-guard branches, called directly —
// missing row and pre-existing row both refuse/skip without a handoff
// call; an already-managed row re-adopts nothing.
func TestAdoptRefusalsDirect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := &countedHandoff{report: FirstRunReport{ControllerSpec: true, Succeeded: true}}

	run := func(state store.ResourceState, seed func(store.Store)) (*Result, error) {
		st := testStore(t)
		if seed != nil {
			seed(st)
		}
		opts := Options{Now: func() time.Time { return time.Now().UTC() }, FirstRun: h}
		res := &Result{Notes: []string{}}
		err := runFirstReconcile(ctx, opts, st, "op_fixtureadopt", res, phaseExtras{})
		return res, err
	}

	// Missing row: info finding, no handoff call.
	h.calls = 0
	res, err := run(store.ResourceBootstrapOwned, nil)
	if err != nil {
		t.Fatalf("missing row: %v", err)
	}
	if h.calls != 0 {
		t.Errorf("missing row must not reach the handoff")
	}
	if !slices.ContainsFunc(res.Skipped, func(s string) bool {
		return strings.Contains(s, "no bootstrap-owned ledger row")
	}) {
		t.Errorf("skipped = %v, want the missing-row finding", res.Skipped)
	}

	// Pre-existing row: typed refusal (adoption never converts it).
	_, err = run(store.ResourcePreExisting, func(st store.Store) {
		if err := st.InsertLedgerEntry(ctx, &store.Resource{
			Kind: inventory.KindApp, Identifier: "core-top-controller",
			Labels: map[string]string{inventory.LabelManaged: inventory.ManagedTrue}, CreatedByOp: "op_fixture", State: store.ResourcePreExisting,
		}); err != nil {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("pre-existing row err = %v, want ErrAdoptionRefused", err)
	}

	// Already managed: the idempotent re-verify — no handoff call.
	h.calls = 0
	res, err = run(store.ResourceManaged, func(st store.Store) {
		if err := st.InsertLedgerEntry(ctx, &store.Resource{
			Kind: inventory.KindApp, Identifier: "core-top-controller",
			Labels: map[string]string{inventory.LabelManaged: inventory.ManagedTrue}, CreatedByOp: "op_fixture", State: store.ResourceManaged,
		}); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("already managed: %v", err)
	}
	if h.calls != 0 {
		t.Errorf("already managed must not re-run the handoff")
	}
	if !slices.ContainsFunc(res.Checked, func(s string) bool {
		return strings.Contains(s, "already managed")
	}) {
		t.Errorf("checked = %v, want the idempotent note", res.Checked)
	}
}

// --- the REAL-pipeline integration (split runner + local git) ---------------
//
// The local-git pattern from internal/cli/recon_test.go: git/tar run for
// REAL against a file:// fixture repo, dokku answers from the FakeRunner,
// and the store is one real temp sqlite shared by install AND the
// reconciler. This is the tier-1 proof that the phase-5 handoff — when
// wired as production does — runs the ADR-0005 adopt-aware pipeline.

const (
	specController = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: top-controller
  namespace: core
spec:
  source:
    image: ghcr.io/example/top/topctl
    tag: bootstrap-pinned
  ports:
    - { name: http, containerPort: 8443 }
  routing:
    mode: internal
  env:
    TZ: UTC
  restartPolicy: always
`
	specWhoami = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: whoami
  namespace: observability
spec:
  source:
    image: ghcr.io/example/whoami
    tag: "1.2.3"
  ports:
    - { name: http, containerPort: 8080 }
  routing:
    mode: internal
  env:
    TZ: UTC
  restartPolicy: always
`
	whoamiImage = "ghcr.io/example/whoami:1.2.3"
)

// installGitShim adapts the exec.Runner seam to gitsource's env-carrying
// Runner (the test-only analog of internal/cli's cliGitShim).
type installGitShim struct{ r exec.Runner }

func (s installGitShim) Run(ctx context.Context, _ []string, name string, args ...string) ([]byte, []byte, error) {
	return s.r.Run(ctx, name, args...)
}

func (s installGitShim) RunStream(ctx context.Context, _ []string, stdout, stderr io.Writer, name string, args ...string) error {
	return s.r.RunStream(ctx, stdout, stderr, name, args...)
}

// reconHandoff is the PRODUCTION-shaped FirstRunner (exactly the
// composition internal/cli must wire; documented in the package README):
// gitsource resolve → fetch → spec.LoadDir → reconcile.Run.
type reconHandoff struct {
	src *gitsource.Source
	be  reconcile.Backend
	st  store.Store
	nss []string
}

// FirstRun implements install.FirstRunner.
func (h reconHandoff) FirstRun(ctx context.Context) (FirstRunReport, error) {
	sha, err := h.src.ResolveLatest(ctx)
	if err != nil {
		return FirstRunReport{}, err
	}
	snap, err := h.src.FetchRevision(ctx, sha)
	if err != nil {
		return FirstRunReport{}, err
	}
	defer func() { _ = snap.Close() }()
	loaded, err := spec.LoadDir(snap.Dir, spec.WithPlatformRepo(true))
	if err != nil {
		return FirstRunReport{}, err
	}
	selfSpec := false
	for _, svc := range loaded.Services {
		if svc.Ref.Equal(ControllerRef()) {
			selfSpec = true
		}
	}
	rec, err := reconcile.New(reconcile.Options{Backend: h.be, Store: h.st})
	if err != nil {
		return FirstRunReport{}, err
	}
	out, err := rec.Run(ctx, reconcile.RunInput{
		Specs:      loaded.Services,
		Revision:   sha,
		Actor:      domain.ActorUser,
		Namespaces: h.nss,
	})
	rep := FirstRunReport{
		Revision:       sha,
		RunID:          "op_" + out.OperationID.String(),
		ControllerSpec: selfSpec,
		Succeeded:      err == nil && out.Status == domain.RunSucceeded,
		Status:         string(out.Status),
	}
	if err != nil {
		return rep, err
	}
	return rep, nil
}

// seedWhoamiTeaches the backend fake the whoami app's observed shape:
// created-and-deployed state matching its spec (so its plan converges
// via field sets, not a deploy), all values fictional.
func seedWhoamiTeaches(f *exectest.FakeRunner) {
	app := "observability-whoami"
	f.SetPrefix([]string{"dokku", "ps:report", app}, respOK(`{"app":"`+app+`","deployed":"true","running":"true","processes":"1"}`))
	f.SetPrefix([]string{"dokku", "git:report", app}, respOK(`{"app":"`+app+`","source-image":"`+whoamiImage+`"}`))
	f.SetPrefix([]string{"dokku", "apps:report", app}, respOK(`{"app":"`+app+`","deploy-source":"docker-image","deploy-source-metadata":"`+whoamiImage+`","locked":"false"}`))
	f.SetPrefix([]string{"dokku", "resource:report", app}, respOK(`{"app":"`+app+`"}`))
	f.SetPrefix([]string{"dokku", "ports:report", app}, respOK(`[]`))
	f.SetPrefix([]string{"dokku", "config:keys", app}, respOK("TZ\n"))
}

// seedReconcileVerbs wires the convergence verbs the first reconciliation
// legitimately runs FOR THE WHOAMI (field sets + network attach): the
// controller itself must produce NONE.
func seedReconcileVerbs(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"dokku", "config:set"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "ports:set"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "domains:set"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "network:set"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "ps:set"}, respOK("1"))
}

// splitExecRunner routes the version-control steps (git, tar) to the REAL
// host binaries and everything else (dokku) to the fake — internal/cli/
// recon_test.go's local-git pattern, mirrored for the install test. It
// carries the same post-create flip as install_test.go's flipRunner, so
// the adapter's Inspect sees "exists" once the bootstrap app is created.
type splitExecRunner struct {
	host exec.Runner
	fl   *exectest.FakeRunner
}

func (s splitExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" || name == "tar" {
		return exec.NewFastExec().Run(ctx, name, args...)
	}
	stdout, stderr, err := s.host.Run(ctx, name, args...)
	flipAfterCreate(s.fl, name, args)
	return stdout, stderr, err
}

func (s splitExecRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	if name == "git" || name == "tar" {
		return exec.NewFastExec().RunStream(ctx, stdout, stderr, name, args...)
	}
	err := s.host.RunStream(ctx, stdout, stderr, name, args...)
	flipAfterCreate(s.fl, name, args)
	return err
}

func seedFixtureRepo(t *testing.T) (URL, wantRev string) {
	t.Helper()
	// Fixture platform repo (local git remote, fictional values).
	repo := t.TempDir() + "/platform-repo"
	if err := os.MkdirAll(repo+"/core", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/core/top-controller.yaml", []byte(specController), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo+"/observability", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/observability/whoami.yaml", []byte(specWhoami), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		all := append([]string{"-C", repo, "-c", "user.name=Fixtures", "-c", "user.email=fixtures@example.invalid"}, args...)
		out, err := stdexec.Command("git", all...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "fixture specs")
	out, err := stdexec.Command("git", "-C", repo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return "file://" + repo, strings.TrimSpace(string(out))
}

// TestInstallAdoptionRealPipeline (tier-1 integration, local git): the
// full install through the REAL gitsource+spec+reconcile handoff —
// adoption records managed, the controller self spec issues NOT ONE
// convergence verb against the controller app, and the other service the
// repo declares is converged (field sets only — its image matches).
func TestInstallAdoptionRealPipeline(t *testing.T) {
	if _, err := stdexec.LookPath("git"); err != nil {
		t.Skip("host git binary unavailable (hermetic tier): skipping the real-pipeline adoption test")
	}
	if _, err := stdexec.LookPath("tar"); err != nil {
		t.Skip("host tar binary unavailable: skipping the real-pipeline adoption test")
	}
	repoURL, wantRev := seedFixtureRepo(t)

	ctx := context.Background()
	f := exectest.NewFakeRunner()
	runner := splitExecRunner{host: f, fl: f}
	st := testStore(t)
	opts := testOptions(t, runner, st, f)
	opts.RepoURL = repoURL
	opts.HealthTimeout = 2 * time.Second // fail fast during fixture shakedown

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)
	seedReconcileVerbs(f)
	seedWhoamiTeaches(f)

	// PRODUCTION handoff composition (mirrors reconcmd's orReconciler):
	// gitsource over the fixture repo (file:// remote, WorkDir under the
	// installer's dataDir/git), the same shared store, the dokku backend.
	opts.FirstRun = reconHandoff{
		src: &gitsource.Source{
			URL:     opts.RepoURL,
			Branch:  branch,
			WorkDir: opts.DataDir + "/git",
			Runner:  installGitShim{r: runner},
		},
		be:  dokku.New(runner),
		st:  st,
		nss: opts.Namespaces,
	}

	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// Adoption evidence.
	if row := controllerAppRow(t, st); row == nil || row.State != store.ResourceManaged {
		t.Fatalf("controller row = %+v, want managed", row)
	}
	if !slices.ContainsFunc(res.Adopted, func(s string) bool {
		return strings.Contains(s, "bootstrap-owned → managed")
	}) {
		t.Errorf("adopted = %v", res.Adopted)
	}
	// The first run's revision is the fixture HEAD, recorded on the op.
	var p5 *domain.Operation
	for _, op := range installOps(t, st) {
		if p, ok := phaseOf(op); ok && p == phaseFirstReconcile && op.State == domain.OpSucceeded {
			p5 = op
		}
	}
	if p5 == nil || p5.Details["firstRunRevision"] != wantRev {
		t.Fatalf("phase-5 op must record the first-run revision %q: %+v", wantRev, p5)
	}

	// The reconciled whoami app is ledgered (the run's ledgerApp).
	rows, lerr := st.ListLedger(ctx, inventory.KindApp)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !slices.ContainsFunc(rows, func(r *store.Resource) bool {
		return r.Identifier == "observability-whoami"
	}) {
		t.Errorf("the reconciled whoami app must be ledgered: %+v", rows)
	}

	// Anti-redeploy: exactly one controller deploy verb (phase 4) and
	// NO reconcile-owned convergence verb ever carries the controller
	// app's name.
	fromImage, rebuilds := groupedDeployVerbs(t, f)
	if fromImage != 1 || rebuilds != 0 {
		t.Errorf("controller deploy verbs: git:from-image=%d rebuild/restart=%d", fromImage, rebuilds)
	}
	for _, c := range f.Recorded() {
		if c.Name != "dokku" || !slices.Contains(c.Args, "core-top-controller") {
			continue
		}
		for _, v := range []string{"config:set", "config:unset", "ports:set", "network:set", "ps:set", "domains:set", "git:sync", "apps:destroy"} {
			if slices.Contains(c.Args, v) {
				t.Fatalf("reconcile-era convergence verb reached the controller app: %s", c.String())
			}
		}
		if slices.Contains(c.Args, "git:from-image") && c.Args[len(c.Args)-1] != opts.ImageRef() {
			t.Fatalf("second controller deploy with a different image: %s", c.String())
		}
	}
}
