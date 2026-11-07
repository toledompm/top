// SPDX-License-Identifier: MIT

package install

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backend"
	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Tier-1 tests (ADR-0012): exectest.FakeRunner + a real migrated temp
// (:memory:) sqlite store. Every fixture value is fictional
// (git.example.com, docker.example.com, the example registry, temp dirs).

const (
	repoURL        = "ssh://git@git.example.com/example/platform.git"
	branch         = "main"
	dkVerOut       = "dokku version 0.38.32 (git commit a11d15d)"
	dockerVer      = "27.0.3"
	imageRef       = "ghcr.io/example/top/topctl:bootstrap-pinned"
	lsRemoteOut    = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678\trefs/heads/main"
	psDeployedJSON = `{"app":"core-top-controller","deployed":"true","running":"true","processes":"1"}`
	etcRelease     = "ID=ubuntu\nVERSION_ID=24.04\n"
	// A foreign container line (the acceptance posture: ownership-safe
	// install/uninstall never touches or ledger-claims foreign state).
	dockerPSForeign = `{"ID":"c9f8e7","Names":"/my-own-service","Image":"docker.example.com/me/my-service:1","State":"running","Labels":""}`
	dockerPSOwn     = `{"ID":"a1b2c3","Names":"/whoami-1","Image":"docker.example.com/example/whoami:1","State":"running","Labels":""}`
	netsForeign     = `{"Name":"bridge","Driver":"bridge","Labels":""}`
	volsForeign     = `{"Name":"vol-not-tops","Driver":"local","Labels":""}`
)

func respOK(out string) exectest.Response { return exectest.Response{Stdout: []byte(out)} }

func respExit(code int) exectest.Response { return exectest.Response{ExitCode: code} }

func respMissing() exectest.Response { return exectest.Response{Err: exec.ErrNotFound} }

// Compile-time: the dokku client satisfies the interface the installer
// consumes (extend-not-fork proof).
var _ backend.PlatformBackend = dokku.New(nil)

// flipRunner flips the dokku apps-exists/app-list probes to "exists" the
// first time an apps:create runs — a real host's symptom without
// FakeRunner call counting. One instance is BOTH Options.Runner and the
// backend's Runner, so the whole install shares one argv log.
type flipRunner struct {
	f *exectest.FakeRunner
}

var _ exec.Runner = (*flipRunner)(nil)

func flipAfterCreate(f *exectest.FakeRunner, name string, args []string) {
	if name == "dokku" && len(args) >= 1 && args[0] == "apps:create" {
		f.SetPrefix([]string{"dokku", "apps:exists"}, respOK("App exists"))
		f.SetPrefix([]string{"dokku", "apps:list"}, respOK(`["core-top-controller"]`))
	}
	// After a (succeeded or not) gated bootstrap attempt, dokku exists —
	// the sudo -n attempt itself settles the fixture.
	if name == "sudo" && slices.Contains(args, "bash") {
		f.SetPrefix([]string{"dokku", "version"}, respOK(dkVerOut))
		f.SetPrefix([]string{"dokku", "apps:list"}, respOK(""))
	}
}

func (e *flipRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	stdout, stderr, err := e.f.Run(ctx, name, args...)
	flipAfterCreate(e.f, name, args)
	return stdout, stderr, err
}

func (e *flipRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	err := e.f.RunStream(ctx, stdout, stderr, name, args...)
	flipAfterCreate(e.f, name, args)
	return err
}

// wrappedRunner returns the shared seam (fake + flip) for one test.
func wrappedRunner(f *exectest.FakeRunner) *flipRunner { return &flipRunner{f: f} }

// testOptions returns production-shaped Options with every test seam
// injected; the dataDir is a temp path (NEVER /var/lib/top), absent
// pre-install (the doctor info fixture).
func testOptions(t *testing.T, r exec.Runner, st store.Store, f *exectest.FakeRunner) Options {
	t.Helper()
	fixtureDir := t.TempDir()
	osRel := filepath.Join(fixtureDir, "os-release")
	if err := os.WriteFile(osRel, []byte(etcRelease), 0o600); err != nil {
		t.Fatal(err)
	}
	deployKey := filepath.Join(fixtureDir, "deploy_key")
	// Generic key-shaped material (never a real PEM block — the hygiene
	// gate forbids literal "BEGIN … PRIVATE KEY" headers, ADR-0014): the
	// v0 installer treats the file as opaque bytes (copy-0600, never
	// parsed), so the fixture only needs to prove the COPY path.
	if err := os.WriteFile(deployKey, []byte("FAKEKEY-PEM-SIMULATION-BEGIN FAKE-KEY-DO-NOT-USE FAKEKEY-PEM-SIMULATION-END\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(fixtureDir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("git.example.com ssh-ed25519 AAAAFAKEPINNEDKEYDO-NOT-USE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		Runner:          r,
		Backend:         dokku.New(r),
		Store:           st,
		DataDir:         filepath.Join(t.TempDir(), "top"),
		RepoURL:         repoURL,
		Branch:          branch,
		Confirm:         true,
		Namespaces:      []string{"core", "observability"},
		ControllerImage: "",
		ControllerTag:   "",
		DeployKey:       deployKey,
		KnownHosts:      knownHosts,

		OSProbe: func() (string, string) { return "linux", "amd64" },
		Probe: func(network, addr string) (func(), error) {
			return func() {}, nil
		},
		MemProbe:      func() (uint64, error) { return 8 << 30, nil },
		DiskProbe:     func(string) (uint64, error) { return 100 << 30, nil },
		OSReleasePath: osRel,

		PollInterval: time.Millisecond, // tests never sleep on health misses
	}
}

func testStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open :memory: store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedDoctor wires the phase-0 probe set. dokkuAbsent flips dokku probes
// to ErrNotFound (the "guided install" posture).
func seedDoctor(f *exectest.FakeRunner, dokkuAbsent bool) {
	if dokkuAbsent {
		f.SetPrefix([]string{"dokku"}, respMissing())
	} else {
		f.SetPrefix([]string{"dokku", "version"}, respOK(dkVerOut))
		f.SetPrefix([]string{"dokku", "apps:list"}, respOK(""))
	}
	f.SetPrefix([]string{"docker", "version"}, respOK(dockerVer))
	f.SetPrefix([]string{"sudo"}, respOK(""))
}

// seedSnapshot wires the read-only snapshot reads: a host whose state is
// foreign-only (bridge network, unrelated volume, someone else's
// container) — ownership rows recorded: NONE (no labels, no prefixes).
func seedSnapshot(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"docker", "ps"}, respOK(dockerPSForeign+"\n"+dockerPSOwn))
	f.SetPrefix([]string{"docker", "network"}, respOK(netsForeign))
	f.SetPrefix([]string{"docker", "volume"}, respOK(volsForeign))
	f.SetPrefix([]string{"ss"}, respOK(""))
}

// seedRepo wires `git ls-remote <repo> <branch>` green (branch non-empty).
func seedRepo(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"git", "ls-remote"}, respOK(lsRemoteOut))
}

// seedPhases wires the mutating-phase dokku verbs plus the health-wait
// report set (deployed+running at the first poll).
func seedPhases(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"dokku", "network:exists"}, respExit(1))
	f.SetPrefix([]string{"dokku", "network:create"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "storage:info"}, respExit(1))
	f.SetPrefix([]string{"dokku", "storage:create"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "apps:exists"}, respExit(20))
	f.SetPrefix([]string{"dokku", "apps:create"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "storage:mount"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "git:from-image"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "ps:report"}, respOK(psDeployedJSON))
	f.SetPrefix([]string{"dokku", "builds:report"}, respOK(`{"app":"core-top-controller","build-status":"succeeded"}`))
	f.SetPrefix([]string{"dokku", "git:report"}, respOK(`{"app":"core-top-controller","source-image":"`+imageRef+`"}`))
	f.SetPrefix([]string{"dokku", "apps:report"}, respOK(`{"app":"core-top-controller","deploy-source":"docker-image","deploy-source-metadata":"`+imageRef+`","locked":"false"}`))
	f.SetPrefix([]string{"dokku", "network:report"}, respOK(`{"app":"core-top-controller","computed-attach-post-create":"top-internal","computed-attach-post-deploy":""}`))
	f.SetPrefix([]string{"dokku", "resource:report"}, respOK(`{"app":"core-top-controller"}`))
	f.SetPrefix([]string{"dokku", "ports:report"}, respOK(`[]`))
	f.SetPrefix([]string{"dokku", "storage:list"}, respOK(`[]`))
	f.SetPrefix([]string{"dokku", "config:keys"}, respOK("TZ\n"))
	f.SetPrefix([]string{"dokku", "domains:report"}, respOK(`{"app-enabled":"false","app-vhosts":"","global-enabled":"false","global-vhosts":""}`))
}

// seedTeardown wires the uninstall verbs (destroy/unmount).
func seedTeardown(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"dokku", "apps:destroy"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "storage:unmount"}, respOK("1"))
	f.SetPrefix([]string{"dokku", "storage:destroy"}, respOK("1"))
}

// mutatingVerbs are the argv tokens that mutate the host: the zero-mutation
// proofs assert NONE of them (nor sudo/wget/ssh-keygen binaries, nor git
// verbs other than ls-remote) appear in the recorded calls.
var mutatingVerbs = []string{
	"apps:create", "apps:destroy", "ps:stop", "ps:rebuild", "ps:restart",
	"network:create", "network:destroy",
	"storage:create", "storage:destroy", "storage:mount", "storage:unmount",
	"git:from-image", "git:sync", "config:set", "config:unset",
}

func recordedArgvOf(f *exectest.FakeRunner) []string {
	out := []string{}
	for _, c := range f.Recorded() {
		out = append(out, c.Name+" "+strings.Join(c.Args, " "))
	}
	return out
}

// mutating binaries (wget runs the upstream script download; sudo must
// only show a probe like `-n true`, never a script/bash/apt invocation).
func assertNoMutatingArgv(t *testing.T, f *exectest.FakeRunner) {
	t.Helper()
	for _, cmd := range f.Recorded() {
		if cmd.Name == "wget" || cmd.Name == "ssh-keygen" {
			t.Fatalf("mutating binary ran: %s", cmd.String())
		}
		if cmd.Name == "sudo" && (len(cmd.Args) != 2 || cmd.Args[0] != "-n" || cmd.Args[1] != "true") {
			t.Fatalf("mutating sudo ran: %s", cmd.String())
		}
		if cmd.Name == "dokku" {
			for _, v := range mutatingVerbs {
				if slices.Contains(cmd.Args, v) {
					t.Fatalf("mutating dokku verb ran: %s", cmd.String())
				}
			}
		}
		if cmd.Name == "git" && !slices.Contains(cmd.Args, "ls-remote") {
			t.Fatalf("mutating git verb ran: %s", cmd.String())
		}
	}
}

// --- read-accessors over the fixtures --------------------------------------------

func installOps(t *testing.T, stx store.Store) []*domain.Operation {
	t.Helper()
	ops, err := stx.ListOperations(context.Background(), 100, &store.OperationFilter{Kind: domain.OpInstall})
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func phaseStateMap(t *testing.T, stx store.Store) map[int]domain.OperationState {
	t.Helper()
	out := map[int]domain.OperationState{}
	for _, op := range installOps(t, stx) {
		p, ok := phaseOf(op)
		if !ok {
			t.Fatalf("install op with no phase detail: %+v", op)
		}
		if _, seen := out[p]; !seen {
			out[p] = op.State // newest first: first per phase wins
		}
	}
	return out
}

func ledgerRows(t *testing.T, stx store.Store, kind string) []*store.Resource {
	t.Helper()
	rows, err := stx.ListLedger(context.Background(), kind)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func readFileMode(t *testing.T, path string) (os.FileMode, string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // fixture path
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm(), string(raw)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// --- the tests --------------------------------------------------------------------

func TestValidateRepoURLShapes(t *testing.T) {
	t.Parallel()
	for _, good := range []string{
		"https://git.example.com/example/platform.git",
		"ssh://git@git.example.com/example/platform.git",
		"git@git.example.com:example/platform.git",
	} {
		if err := validateRepoURL(good); err != nil {
			t.Errorf("%q: refused, want accepted: %v", good, err)
		}
	}
	for _, bad := range []string{
		"github.com/example/top",
		"",
		"git.example.com/example/top",
	} {
		if err := validateRepoURL(bad); err == nil {
			t.Errorf("%q: accepted, want refused (not a plausible git remote)", bad)
		}
	}
}

// TestInstallGateRefused: the package refuses to mutate without the
// ADR-0009 approval marker; the CLI's --confirm/interactive prompt is
// what produces it. Zero argv touches the runner on the refusal path.
func TestInstallGateRefused(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, wrappedRunner(f), st, f)
	opts.Confirm = false
	seedDoctor(f, false)
	seedRepo(f)

	res, err := Install(context.Background(), opts)
	if !errors.Is(err, ErrGateRequired) {
		t.Fatalf("err = %v, want ErrGateRequired", err)
	}
	if res != nil {
		t.Fatalf("res = %+v, want nil (no partial result from a refused run)", res)
	}
	if got := recordedArgvOf(f); len(got) != 0 {
		t.Fatalf("refused invocation ran argv: %v", got)
	}
}

func TestInstallBadRepoURLShape(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, f, st, f)
	opts.RepoURL = "github.com/example/top"
	if _, err := Install(context.Background(), opts); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("err = %v, want domain.ErrInvalid-class repo shape refusal", err)
	}
}

// TestInstallDryRunZeroMutations: full plan printed, nothing written —
// no store ops, no ledger rows, no filesystem changes, no mutating argv.
func TestInstallDryRunZeroMutations(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, f, st, f)
	opts.DryRun = true
	opts.Confirm = false // dry-run needs no approval: nothing mutates

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)
	seedTeardown(f)

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install dry-run: %v", err)
	}
	if len(res.PhasesRun) != 7 {
		t.Fatalf("dry-run plan phases = %v, want the 7 phase lines", res.PhasesRun)
	}
	if !res.Preflight.OK() {
		t.Errorf("dry-run must attach a green preflight report (read-only run of phase 0): %+v", res.Preflight.Findings)
	}
	assertNoMutatingArgv(t, f)
	if got := dirNames(t, opts.DataDir); len(got) != 0 {
		t.Fatalf("dry-run created under the data dir: %v", got)
	}
	if ops := installOps(t, st); len(ops) != 0 {
		t.Fatalf("dry-run recorded operations: %v", ops)
	}
	if rows := ledgerRows(t, st, ""); len(rows) != 0 {
		t.Fatalf("dry-run wrote ledger rows: %v", rows)
	}
	if !slices.ContainsFunc(res.Notes, func(n string) bool {
		return strings.Contains(n, "adoption note")
	}) {
		t.Fatalf("dry-run plan must carry the adoption-record note: %v", res.Notes)
	}
}

// TestInstallFullPhaseSequence: the tier-1 integration — preflight,
// inventory, dokku verify, wiring, bootstrap deploy, report over a temp
// sqlite; every creation lands as a bootstrap-owned ledger row and every
// phase completes its operation record.
func TestInstallFullPhaseSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	opts := testOptions(t, r, st, f)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	dataDir := opts.DataDir

	// Directory tree: 0700.
	for _, sub := range []string{"", "secrets", "git"} {
		stt, err := os.Stat(filepath.Join(dataDir, sub))
		if err != nil {
			t.Fatalf("dir %s missing after install: %v", sub, err)
		}
		if stt.Mode().Perm() != 0o700 {
			t.Errorf("dir %s mode = %o, want 700", sub, stt.Mode().Perm())
		}
	}
	// Secret-material file copies: 0600.
	for _, name := range []string{"deploy_key", "known_hosts"} {
		if perm, _ := readFileMode(t, filepath.Join(dataDir, "git", name)); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}

	// The golden mutating argv: the installer speaks ONLY the backend
	// interface; the adapter owns the syntax.
	got := recordedArgvOf(f)
	want := []string{
		"dokku network:exists top-internal",
		"dokku network:create top-internal",
		"dokku storage:info top-metadata",
		"dokku storage:create top-metadata " + dataDir + " --chown false",
		"dokku apps:exists core-top-controller",
		"dokku apps:create core-top-controller",
		"dokku storage:mount core-top-controller top-metadata --container-dir " + dataDir,
		"dokku git:from-image core-top-controller " + imageRef,
	}
	for _, line := range want {
		if !slices.Contains(got, line) {
			t.Errorf("argv missing %q; got:\n%s", line, strings.Join(got, "\n"))
		}
	}

	// Ledger: the two bootstrap assets, anchored + labeled.
	rows := ledgerRows(t, st, "")
	var netRow, appRow *store.Resource
	for _, row := range rows {
		switch {
		case row.Kind == inventory.KindNetwork && row.Identifier == "top-internal":
			netRow = row
		case row.Kind == inventory.KindApp && row.Identifier == "core-top-controller":
			appRow = row
		default:
			t.Errorf("unexpected/non-TOP ledger row after install: %+v", row)
		}
	}
	if netRow == nil {
		t.Fatalf("network ledger row missing (rows=%+v)", rows)
	}
	if netRow.State != store.ResourceBootstrapOwned || netRow.CreatedByOp == "" {
		t.Errorf("network row = %+v, want bootstrap-owned with op anchor", netRow)
	}
	if appRow == nil {
		t.Fatalf("app ledger row missing")
	}
	if appRow.State != store.ResourceBootstrapOwned ||
		appRow.Labels[inventory.LabelManaged] != inventory.ManagedTrue {
		t.Errorf("app row = %+v, want bootstrap-owned with top.dev labels", appRow)
	}

	// Operation records: all 6 phases succeeded.
	states := phaseStateMap(t, st)
	for p := range phaseNames {
		if states[p] != domain.OpSucceeded {
			t.Errorf("phase %s state = %v, want succeeded", phaseNames[p], states[p])
		}
	}
	// The phase-1 op embeds the (size-capped) snapshot payload.
	for _, op := range installOps(t, st) {
		if p, ok := phaseOf(op); ok && p == phaseInventory && op.State == domain.OpSucceeded {
			if op.Details["inventorySnapshot"] == nil {
				t.Errorf("phase-1 details must embed the snapshot: %v", op.Details)
			}
		}
	}

	// Report lists reflect reality.
	if !slices.Contains(res.Created, "app core-top-controller") ||
		!slices.Contains(res.Created, "network top-internal") {
		t.Errorf("created list incomplete: %v", res.Created)
	}
	if len(res.Adopted) != 0 {
		t.Errorf("clean install must adopt nothing: %v", res.Adopted)
	}
}

// TestInstallResumesAfterFailedBootstrap (the contract's failure
// behavior): a failed bootstrap deploy leaves prior phases untouched,
// records the bootstrap-failed marker, and the re-run skips completed
// phases and adopts the leftover app to finish.
func TestInstallResumesAfterFailedBootstrap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	opts := testOptions(t, r, st, f)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)
	f.SetPrefix([]string{"dokku", "git:from-image"}, respExit(1)) // deploy fails

	res1, err := Install(ctx, opts)
	if err == nil {
		t.Fatal("run 1 must fail at the bootstrap deploy")
	}
	if !strings.Contains(err.Error(), "bootstrap-failed") {
		t.Errorf("run-1 err must carry the bootstrap-failed marker: %v", err)
	}
	if res1 == nil || len(res1.Created) == 0 {
		t.Errorf("run-1 must return the partial Result for the printout (wiring happened): %+v", res1)
	}
	// Prior phases untouched: the wiring exists, nothing was removed.
	got := dirNames(t, opts.DataDir)
	if !slices.Contains(got, "secrets") || !slices.Contains(got, "git") {
		t.Errorf("run-1 must leave phases 1..3's effects in place: %v", got)
	}

	ops := installOps(t, st)
	var op4 *domain.Operation
	for _, op := range ops {
		if p, ok := phaseOf(op); ok && p == phaseBootstrapDeploy && op.State == domain.OpFailed {
			op4 = op
			break
		}
	}
	if op4 == nil || !strings.Contains(op4.Err, "bootstrap-failed") {
		t.Errorf("phase-4 failed op must record the marker, got %+v", op4)
	}

	// Re-run with the deploy seed green.
	f.SetPrefix([]string{"dokku", "git:from-image"}, respOK("1"))
	res2, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	wantSkipped := []string{phaseNames[phaseInventory], phaseNames[phaseDokkuSetup], phaseNames[phaseWiring]}
	if !slices.Equal(res2.PhasesSkipped, wantSkipped) {
		t.Errorf("re-run skipped = %v, want %v", res2.PhasesSkipped, wantSkipped)
	}
	if !res2.Resumed {
		t.Errorf("re-run must report Resumed")
	}
	if len(res2.Adopted) == 0 {
		t.Errorf("re-run must adopt the leftover app: %v", res2.Adopted)
	}
	states := phaseStateMap(t, st)
	if states[phaseBootstrapDeploy] != domain.OpSucceeded {
		t.Errorf("phase-4 after re-run = %v, want succeeded", states[phaseBootstrapDeploy])
	}
}

// TestInstallResumesInterruptedRun: a SIGKILLed invocation left a phase-4
// operation RUNNING; the next install aborts the stale op and redoes the
// phase (recovery-safe interruption).
func TestInstallResumesInterruptedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	opts := testOptions(t, r, st, f)

	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)
	seedTeardown(f)

	// Seed the interrupted state: phases 0-3 succeeded, phase 4 running.
	now := time.Now().UTC()
	for _, ph := range []int{phasePreflight, phaseInventory, phaseDokkuSetup, phaseWiring} {
		op, err := domain.NewOperation(domain.OpInstall, domain.ActorUser, "", "")
		if err != nil {
			t.Fatal(err)
		}
		op.Details = map[string]any{"phase": ph}
		if err := st.InsertOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", now); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
	}
	stale, err := domain.NewOperation(domain.OpInstall, domain.ActorUser, "", "")
	if err != nil {
		t.Fatal(err)
	}
	stale.Details = map[string]any{"phase": phaseBootstrapDeploy}
	if err := st.InsertOperation(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOperationState(ctx, stale.ID, domain.OpRunning, "", now); err != nil {
		t.Fatal(err)
	}

	res, err := Install(ctx, opts)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !res.Resumed {
		t.Errorf("resume must flag Resumed")
	}
	if !slices.Equal(res.PhasesSkipped, []string{phaseNames[phaseInventory], phaseNames[phaseDokkuSetup], phaseNames[phaseWiring]}) {
		t.Errorf("resume skipped = %v, want phases 1-3", res.PhasesSkipped)
	}

	// The stale op is aborted; a fresh phase-4 record succeeded.
	var staleAborted, deploySucceeded bool
	for _, op := range installOps(t, st) {
		if op.ID == stale.ID && op.State == domain.OpAborted {
			staleAborted = true
		}
		if p, ok := phaseOf(op); ok && p == phaseBootstrapDeploy && op.State == domain.OpSucceeded {
			deploySucceeded = true
		}
	}
	if !staleAborted {
		t.Errorf("the stale RUNNING phase-4 op must be aborted")
	}
	if !deploySucceeded {
		t.Errorf("the redone phase-4 must succeed and record itself")
	}
}

// TestInstallPreflightFailAborts: fail findings abort with
// ErrPreflightFailed before ANY mutating argv runs, and the doctor
// report rides on the Result for the printout.
func TestInstallPreflightFailAborts(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, f, st, f)

	seedDoctor(f, false)
	f.SetPrefix([]string{"git", "ls-remote"}, respExit(128)) // repo unreachable

	res, err := Install(context.Background(), opts)
	if !errors.Is(err, ErrPreflightFailed) {
		t.Fatalf("err = %v, want ErrPreflightFailed", err)
	}
	found := false
	for _, fnd := range res.Preflight.Findings {
		if fnd.ID == "repo/reachable" && fnd.Sev() == doctor.SeverityFail {
			found = true
		}
	}
	if !found {
		t.Errorf("preflight report must carry the repo finding: %+v", res.Preflight.Findings)
	}
	assertNoMutatingArgv(t, f)
	if rows := ledgerRows(t, st, ""); len(rows) != 0 {
		t.Errorf("nothing may be ledgered on a preflight abort: %v", rows)
	}
}

// TestInstallDokkuRequiredRefusal: dokku absent + no --install-dokku →
// the clean instructed stop (no verbs beyond the probes).
func TestInstallDokkuRequiredRefusal(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, f, st, f)

	seedDoctor(f, true) // dokku absent
	seedRepo(f)
	seedSnapshot(f)

	_, err := Install(context.Background(), opts)
	if !errors.Is(err, ErrDokkuRequired) {
		t.Fatalf("err = %v, want ErrDokkuRequired", err)
	}
	if !strings.Contains(err.Error(), "--install-dokku") {
		t.Errorf("refusal must instruct about --install-dokku: %v", err)
	}
	assertNoMutatingArgv(t, f)
}

// TestInstallDokkuBootstrapGoldenArgv: dokku absent + --install-dokku →
// the documented pinned upstream bootstrap; sudo runs NON-interactively
// (`sudo -n …` — the deterministic attempt argv).
func TestInstallDokkuBootstrapGoldenArgv(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	r := wrappedRunner(f)
	st := testStore(t)
	opts := testOptions(t, r, st, f)
	opts.InstallDokku = true

	seedDoctor(f, true)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatalf("install --install-dokku: %v", err)
	}
	got := recordedArgvOf(f)
	for _, want := range []string{
		"wget -NP . " + dokkuBootstrapScriptURL,
		"sudo -n DOKKU_TAG=" + dokkuBootstrapTag + " bash " + bootstrapScriptFileName,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("gated-dokku argv missing %q; got:\n%s", want, strings.Join(got, "\n"))
		}
	}
}

// TestInstallDokkuSudoPasswordRefusal: passwordless sudo unavailable in
// the run session (doctor's interactive-sudo semantics): the typed
// refusal prints the exact fallback commands.
func TestInstallDokkuSudoPasswordRefusal(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, f, st, f)
	opts.InstallDokku = true

	seedDoctor(f, true)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)
	seedTeardown(f)
	f.SetPrefix([]string{"sudo"}, respExit(1)) // sudo -n refused (password required)

	_, err := Install(context.Background(), opts)
	if !errors.Is(err, ErrSudoRefused) {
		t.Fatalf("err = %v, want ErrSudoRefused", err)
	}
	for _, want := range sudoScriptCommands() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must print the exact command %q: %v", want, err)
		}
	}
	got := recordedArgvOf(f)
	if !slices.Contains(got, "sudo -n DOKKU_TAG="+dokkuBootstrapTag+" bash "+bootstrapScriptFileName) {
		t.Errorf("the sudo -n attempt argv must be recorded")
	}
}

// TestInstallDokkuSudoMissing: sudo not on PATH — the print path fires
// with instructions (never a crash, never a partial install).
func TestInstallDokkuSudoMissing(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testOptions(t, f, st, f)
	opts.InstallDokku = true

	seedDoctor(f, true)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)
	f.SetPrefix([]string{"sudo"}, respMissing())

	_, err := Install(context.Background(), opts)
	if !errors.Is(err, ErrSudoRefused) {
		t.Fatalf("err = %v, want ErrSudoRefused (caught + printed)", err)
	}
}
