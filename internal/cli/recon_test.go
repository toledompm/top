// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	stdexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// T-309: the `topctl diff` / `topctl reconcile` / `topctl deploy` bodies
// are real. These tests drive them through runCLIOpts with a split runner:
// git/tar go to the REAL local binaries against a committable fixture
// platform repo (one spec file, fictional values — never the network), and
// every other command (dokku) answers from a scripted FakeRunner. The
// state store is a real SQLite file created under the test's own dataDir.
// No TTY anywhere; mutating gates are exercised without --confirm in the
// non-destructive rows and with the contract rows in cli_test.go.
//
// Tier note: the real-git half needs the host `git`/`tar` binaries (same
// tier as doctor's read-only host use, documented in internal/cli/README);
// the suite skips cleanly when git is absent (hermetic CI tier).

// testRev is the fixture revision (a well-formed 40-hex sha).
const testRev = "ac00000000000000000000000000000000000000"

// webSpec is the one fictional spec committed to the fixture repo.
const webSpec = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: web
  namespace: tops
spec:
  source:
    image: ghcr.io/example/web:1.2.3
  ports:
    - { name: http, containerPort: 8080 }
  routing:
    mode: internal
  env:
    TZ: UTC
  restartPolicy: always
`

// splitRunner routes the version-control steps (args + tar extraction) to
// the real host binaries and everything else (dokku) to the fake. Without
// this the fake would swallow `tar -xf` and the snapshot dir would stay
// empty — spec loading needs real files on disk.
type splitRunner struct {
	host exec.Runner
}

func (s splitRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" || name == "tar" {
		return exec.NewFastExec().Run(ctx, name, args...)
	}
	return s.host.Run(ctx, name, args...)
}

func (s splitRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	if name == "git" || name == "tar" {
		return exec.NewFastExec().RunStream(ctx, stdout, stderr, name, args...)
	}
	return s.host.RunStream(ctx, stdout, stderr, name, args...)
}

// setUpPlatformRepo creates a local git repo holding webSpec on `main` and
// returns its path (used directly as the config's git.url — a file remote).
func setUpPlatformRepo(t *testing.T) string {
	t.Helper()
	if _, err := stdexec.LookPath("git"); err != nil {
		t.Skip("host git binary unavailable (hermetic tier): skipping T-309 CLI wiring tests")
	}
	dir := t.TempDir() + "/platform-repo"
	if err := os.MkdirAll(dir+"/tops", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/tops/web.yaml", []byte(webSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmdArgs := append([]string{"-C", dir, "-c", "user.name=Fixtures", "-c", "user.email=fixtures@example.invalid"}, args...)
		out, err := stdexec.Command("git", cmdArgs...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("add", ".")
	runGit("commit", "--quiet", "-m", "fixture spec")
	return dir
}

// seedAppMissing teaches the fake that the fixture app does not exist
// (dokku's documented "any non-zero exit means not exist", verified 20).
func seedAppMissing(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"dokku", "apps:exists"}, exectest.Response{ExitCode: 20, Stderr: []byte("!  App top-web does not exist")})
}

// writeConfig pins dataDir into a temp config (the CLI's --config path)
// with the given platform-repo URL and returns the config file path.
func writeConfig(t *testing.T, tmp, repoURL string) string {
	t.Helper()
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	yaml := "apiVersion: top/v1alpha1\nlogLevel: info\ndataDir: " + tmp + "\n" +
		"git:\n  url: " + repoURL + "\n  branch: main\n"
	cfg := tmp + "/top-config.yaml"
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// reconFixture wires one CLI invocation's whole environment: local platform
// repo, temp dataDir for config+store+mirror, fake dokku backend (app
// missing unless seeded otherwise). Returned are the fake (for extra
// seeding before args run) and the config path; run with args().
type reconFixture struct {
	f        *exectest.FakeRunner
	cfgPath  string
	dataDir  string
	repoURL  string
	repoHEAD string
	skipGit  bool
}

func newReconFixture(t *testing.T, withGit bool) *reconFixture {
	t.Helper()
	f := exectest.NewFakeRunner()
	fx := &reconFixture{f: f, skipGit: true}
	if withGit {
		fx.repoURL = setUpPlatformRepo(t)
		fx.repoHEAD = gitHead(t, fx.repoURL)
		fx.skipGit = false
	}
	fx.dataDir = t.TempDir()
	// NOTE: no ensureMirror pre-seed on the real-git path — the mirror
	// must be a REAL clone of the fixture repo, so EnsureMirror's
	// `git clone --mirror` runs for real there.
	fx.cfgPath = writeConfig(t, fx.dataDir, fx.repoURL)
	return fx
}

// gitHead returns the fixture repo's HEAD sha via the host git.
func gitHead(t *testing.T, repo string) string {
	t.Helper()
	out, err := stdexec.Command("git", "-C", repo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse head: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// runCLIArgs runs the CLI with this fixture's runner (+args appended with
// --config so every invocation lands in the fixture dataDir).
func (fx *reconFixture) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	// The split runner is ALWAYS correct here: with a fixture repo the
	// version-control steps run for real, without one nothing calls git
	// at all (history-only rows).
	runner := Options{Runner: splitRunner{host: fx.f}}
	base := append([]string{"--config", fx.cfgPath}, args...)
	return runCLIOpts(t, runner, base...)
}

// openFixtureStore opens the SQLite store at the fixture's dataDir for
// direct seeding (history rows, held leases).
func (fx *reconFixture) openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), fx.dataDir+"/top.db")
	if err != nil {
		t.Fatalf("open fixture store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// staleLease holds the reconcile lease for a foreign holder so a pipeline
// run hits the typed LeaseConflictError.
func (fx *reconFixture) staleLease(t *testing.T) {
	t.Helper()
	st := fx.openStore(t)
	ok, err := st.AcquireLease(context.Background(), "reconcile", "doppel-controller", time.Minute, false)
	if err != nil || !ok {
		t.Fatalf("seed lease: ok=%v err=%v", ok, err)
	}
}

// writeSpeclessRepo makes a committed fixture repo with NO spec files
// (the clean-diff case: nothing desired, nothing orphaned).
func writeSpeclessRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := stdexec.LookPath("git"); err != nil {
		t.Skip("host git binary unavailable (hermetic tier): skipping T-309 CLI wiring tests")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmdArgs := append([]string{"-C", dir, "-c", "user.name=Fixtures", "-c", "user.email=fixtures@example.invalid"}, args...)
		out, err := stdexec.Command("git", cmdArgs...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit("init", "-b", "main")
	runGit("commit", "--quiet", "--allow-empty", "-m", "empty platform repo")
}

// ---------------------------------------------------------------------------
// diff
// ---------------------------------------------------------------------------

// TestDiffSpeclessClean: a repo with zero specs over a host with no apps
// and no store → zero rows, exit 0 (the contract's script anchor: "after a
// green run the same diff exits 0"). Uses the REAL local git/git-only
// fixture path (no dokku call expected: no desired services, no ledger).
func TestDiffSpeclessClean(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := tmp + "/platform-repo"
	writeSpeclessRepo(t, repo)
	f := exectest.NewFakeRunner()
	cfgPath := writeConfig(t, t.TempDir(), repo)
	outS, errS, code := runCLIOpts(t, Options{Runner: splitRunner{host: f}}, "--config", cfgPath, "diff", "-o", "json")
	if code != ExitOK {
		t.Fatalf("diff exit = %d, want 0 (out %q err %q)", code, outS, errS)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("diff json: %q: %v", outS, err)
	}
	services, _ := env.Data["services"].([]any)
	if len(services) != 0 {
		t.Fatalf("services = %v, want none", services)
	}
}

// TestDiffDriftCreateExit1: one spec, app not deployed → create row with
// the spec's file:line source; contract exit 1 (drift space).
func TestDiffDriftCreateExit1(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	seedAppMissing(fx.f)
	outS, errS, code := fx.run(t, "diff", "--revision", fx.repoHEAD, "-o", "json")
	if code != ExitDrift {
		t.Fatalf("diff exit = %d, want 1 (out %q err %q)", code, outS, errS)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("diff json: %q: %v", outS, err)
	}
	services, _ := env.Data["services"].([]any)
	if len(services) != 1 {
		t.Fatalf("services = %v, want one create row", env.Data["services"])
	}
	row := services[0].(map[string]any)
	if row["service"] != "tops/web" || row["change"] != "create" {
		t.Fatalf("row = %v", row)
	}
	if src, _ := row["source"].(string); !strings.Contains(src, "tops/web.yaml:") {
		t.Fatalf("source = %q, want tops/web.yaml:<line>", src)
	}
}

// TestDiffRevisionGone: a sha that exists nowhere on the remote → the
// gitsource fetch failure mapped to exit 4 (unavailable space); the real
// git's "not our ref" wording carries no unreachable signature, so the
// class is git-internal — the important property is the exit space, not
// the sub-code of one git build's phrasing.
func TestDiffRevisionGone(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	_, errS, code := fx.run(t, "diff", "--revision", testRev, "-o", "json")
	if code != ExitUnreachable {
		t.Fatalf("exit = %d, want 4 (err %q)", code, errS)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if !strings.HasPrefix(env.Error.Code, "git-") {
		t.Fatalf("code = %q", env.Error.Code)
	}
}

// TestDiffGitUnreachable: ls-remote against a port on localhost nothing
// listens on (a real ECONNREFUSED, loopback-only) → exit 4 with a git
// unavailability code (the real git build's phrasing does not match the
// unreachable stderr signatures here, so assert the exit space and the
// git- classification prefix).
func TestDiffGitUnreachable(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := tmp + "/platform-repo"
	writeSpeclessRepo(t, repo)
	// Point git at a port on localhost that nothing will listen on: a real
	// ECONNREFUSED — no network beyond the loopback.
	cfgPath := writeConfig(t, t.TempDir(), "https://127.0.0.1:1/example/example-repo.git")
	f := exectest.NewFakeRunner()
	_, errS, code := runCLIOpts(t, Options{Runner: splitRunner{host: f}}, "--config", cfgPath, "diff", "-o", "json")
	if code != ExitUnreachable {
		t.Fatalf("exit = %d (err %q)", code, errS)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if !strings.HasPrefix(env.Error.Code, "git-") {
		t.Fatalf("code = %q (message %q)", env.Error.Code, env.Error.Message)
	}
}

// TestDiffInvalidSpecExit3: a spec with a field outside the strict field
// set fails LoadDir → exit 3, code "validation-failed" (spec content is
// data, not argv).
func TestDiffInvalidSpecExit3(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	seedAppMissing(fx.f)
	// commit a second, invalid spec into the same fixture repo (different
	// name: identity uniqueness must stay out of the way — the assertion
	// targets the unknown field, not a duplicate error)
	broken := fx.repoURL + "/tops/broken.yaml"
	if err := os.WriteFile(broken, []byte(strings.Replace(webSpec, "name: web\n", "name: broken\n", 1)+"bogusField: yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitQuiet(t, fx.repoURL, "add", ".")
	runGitQuiet(t, fx.repoURL, "-c", "user.name=Fixtures", "-c", "user.email=fixtures@example.invalid", "commit", "--quiet", "--amend", "-m", "broken spec")
	fx.repoHEAD = gitHead(t, fx.repoURL)

	_, errS, code := fx.run(t, "diff", "--revision", fx.repoHEAD, "-o", "json")
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 3 (err %q)", code, errS)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if env.Error.Code != "validation-failed" || !strings.Contains(env.Error.Message, "bogusField") {
		t.Fatalf("code = %q msg = %q", env.Error.Code, env.Error.Message)
	}
}

// runGitQuiet is setUpPlatformRepo's helper, reusable for edits.
func runGitQuiet(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	out, err := stdexec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// ---------------------------------------------------------------------------
// reconcile
// ---------------------------------------------------------------------------

// TestReconcileNoStoreExit4: the reconciler pipeline requires a state
// store (it records the operation); on a fresh host without top.db
// the wiring refuses with the store-unavailable class → exit 4, code
// "reconciler-unavailable" — never a half-run without bookkeeping.
func TestReconcileNoStoreExit4(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	_, errS, code := fx.run(t, "reconcile", "-o", "json")
	if code != ExitUnreachable {
		t.Fatalf("exit = %d, want 4 (err %q)", code, errS)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if env.Error.Code != "reconciler-unavailable" {
		t.Fatalf("code = %q (msg %q)", env.Error.Code, env.Error.Message)
	}
}

// TestReconcileDryRunPrintsPlan: docs/contracts/cli.md invariant "every
// mutating command supports --dry-run printing the plan" — reconcile
// --dry-run over an undeployed spec prints the create plan (same shared
// plan the executed run would consume) and exits 0; the plan's exit code
// is deliberately not the diff command's drift channel.
func TestReconcileDryRunPrintsPlan(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	seedAppMissing(fx.f)
	outS, errS, code := fx.run(t, "reconcile", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d (out %q err %q)", code, outS, errS)
	}
	if !strings.Contains(outS, "tops/web: create") {
		t.Fatalf("plan missing create row:\n%s", outS)
	}
	if !strings.Contains(outS, "tops/web.yaml:") {
		t.Fatalf("plan missing the file:line source:\n%s", outS)
	}
}

// TestReconcileDryRunJSON: the same dry-run in -o json prints the ver-
// sioned envelope with the diff-shaped payload.
func TestReconcileDryRunJSON(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	seedAppMissing(fx.f)
	outS, _, code := fx.run(t, "reconcile", "--dry-run", "-o", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("not one JSON object: %q", outS)
	}
	if env.SchemaVersion != JSONSchemaVersion || env.Command != "reconcile" {
		t.Fatalf("envelope = %+v", env)
	}
	if services, _ := env.Data["services"].([]any); len(services) != 1 {
		t.Fatalf("services = %v", env.Data["services"])
	}
}

// TestReconcileDryRunPruneNotAllowed: delete rows are "prune only" — with
// no --prune the orphan report may still appear but the dry-run plan must
// not carry delete rows for unconfirmed prunes (here: none — nothing
// orphaned). A --prune dry-run without --confirm is refused by the
// requireConfirm gate (exit 2, pinned in cli_test.go).
func TestReconcileDryRunPruneNotAllowed(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	fx.f.SetPrefix([]string{"dokku"}, exectest.Response{Stdout: []byte("tops-web\n")})
	seedAppMissing(fx.f)
	outS, errS, code := fx.run(t, "reconcile", "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d (out %q err %q)", code, outS, errS)
	}
	if strings.Contains(outS, "delete") {
		t.Fatalf("unconfirmed prune must not delete; text:\n%s", outS)
	}
}

// TestReconcileLeaseConflictExit3: the reconcile lease held by another
// holder → typed LeaseConflictError; docs/contracts/cli.md has no
// dedicated row, so the contract's operation-failed space (3) with
// envelope code "lease-conflict" applies (documented in the wiring).
func TestReconcileLeaseConflictExit3(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	fx.staleLease(t)
	_, errS, code := fx.run(t, "reconcile", "-o", "json")
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 3 (err %q)", code, errS)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil {
		t.Fatalf("stderr not an error envelope: %q", errS)
	}
	if env.Error.Code != "lease-conflict" || !strings.Contains(env.Error.Message, "doppel-controller") {
		t.Fatalf("code = %q msg = %q", env.Error.Code, env.Error.Message)
	}
}

// TestReconcileUnknownServiceExit2: `--service` names a service no spec
// defines → the operator's ask is wrong: exit 2, never a recorded run.
func TestReconcileUnknownServiceExit2(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	_, errS, code := fx.run(t, "reconcile", "--service", "tops/nope", "-o", "json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2 (err %q)", code, errS)
	}
	var env ErrEnvelope
	if json.Unmarshal([]byte(errS), &env) == nil && env.Error.Code != "usage" {
		t.Fatalf("code = %q", env.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// deploy
// ---------------------------------------------------------------------------

// TestDeployDryRun: the deploy row's plan preview — same shared plan,
// pinned revision, exit 0.
func TestDeployDryRun(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	seedAppMissing(fx.f)
	outS, errS, code := fx.run(t, "deploy", "tops/web", "--revision", fx.repoHEAD, "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d (out %q err %q)", code, outS, errS)
	}
	if !strings.Contains(outS, "tops/web: create") {
		t.Fatalf("plan missing create row:\n%s", outS)
	}
}

// TestDeployPullsDependencies: a service whose spec dependsOn another is
// deployed TOGETHER with that dependency (docs/contracts/cli.md deploy
// row: Reconciler (single service) — one converge, not a bare-slice that
// would fail on an unresolved dependency). Assert via theLittle plan rows,
// using a --dry-run (no mutations: read-only assertion surface).
func TestDeployPullsDependencies(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := tmp + "/platform-repo"
	if _, err := stdexec.LookPath("git"); err != nil {
		t.Skip("host git binary unavailable")
	}
	if err := os.MkdirAll(repo+"/tops", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/tops/web.yaml", []byte(webSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/tops/edge.yaml", []byte(strings.Replace(strings.Replace(webSpec, "name: web\n", "name: edge\n", 1), "image: ghcr.io/example/web:1.2.3", "image: ghcr.io/example/edge:9.9.9", 1)+"  dependsOn:\n    - tops/web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := exectest.NewFakeRunner()
	seedAppMissing(f)
	cfgPath := writeConfig(t, tmp+"/data", repo)
	runGitQuiet(t, repo, "init", "-b", "main")
	runGitQuiet(t, repo, "add", ".")
	runGitQuiet(t, repo, "-c", "user.name=Fixtures", "-c", "user.email=fixtures@example.invalid", "commit", "--quiet", "-m", "fixture")
	head := gitHead(t, repo)
	outS, errS, code := runCLIOpts(t, Options{Runner: splitRunner{host: f}},
		"--config", cfgPath, "deploy", "tops/edge", "--revision", head, "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit = %d (out %q err %q)", code, outS, errS)
	}
	for _, wantRow := range []string{"tops/web: create", "tops/edge: create"} {
		if !strings.Contains(outS, wantRow) {
			t.Fatalf("plan missing %q:\n%s", wantRow, outS)
		}
	}
}

// TestDeployUnknownServiceExit2: deploying a service no spec defines.
func TestDeployUnknownServiceExit2(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	_, errS, code := fx.run(t, "deploy", "tops/nope", "--dry-run", "-o", "json")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want 2 (err %q)", code, errS)
	}
	var env ErrEnvelope
	if err := json.Unmarshal([]byte(errS), &env); err != nil || env.Error.Code != "usage" {
		t.Fatalf("code = %q", env.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// history
// ---------------------------------------------------------------------------

// seedHistoryOps inserts one succeeded op and a failed one naming
// tops/web (the reconciler's redacted "ns/name: reason" shape) into the
// fixture store.
func seedHistoryOps(t *testing.T, fx *reconFixture) (succeededID, failedID string) {
	t.Helper()
	st := fx.openStore(t)
	ctx := context.Background()
	mk := func(kind domain.OperationKind, rev, failText string) string {
		op, err := domain.NewOperation(kind, domain.ActorUser, "", rev)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.InsertOperation(ctx, op); err != nil {
			t.Fatal(err)
		}
		// The store mirrors the lifecycle: pending → running → terminal,
		// each transition hitting UpdateOperationState at its point.
		if err = op.Start(); err != nil {
			t.Fatal(err)
		}
		if err = st.UpdateOperationState(ctx, op.ID, domain.OpRunning, "", op.StartedAt); err != nil {
			t.Fatal(err)
		}
		if failText == "" {
			if err = op.Succeed(); err != nil {
				t.Fatal(err)
			}
			if err = st.UpdateOperationState(ctx, op.ID, domain.OpSucceeded, "", op.FinishedAt); err != nil {
				t.Fatal(err)
			}
		} else {
			if err = op.Fail(failText); err != nil {
				t.Fatal(err)
			}
			if err = st.UpdateOperationState(ctx, op.ID, domain.OpFailed, op.Err, op.FinishedAt); err != nil {
				t.Fatal(err)
			}
			return op.ID.String()
		}
		return op.ID.String()
	}
	succeededID = mk(domain.OpReconcile, testRev, "")
	time.Sleep(2 * time.Millisecond) // deterministic newer-first ordering
	failedID = mk(domain.OpDeploy, testRev, "tops/web: image pull failed (redacted)")
	return succeededID, failedID
}

// TestHistoryRealStore: the real Store-backed listing — rows newest
// first, the failed row carrying its actionable `topctl logs` hint
// (the T-308 hint vocabulary's history half).
func TestHistoryRealStore(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, false) // no git needed: history touches only the store
	succeededID, failedID := seedHistoryOps(t, fx)
	outS, errS, code := fx.run(t, "history")
	if code != ExitOK {
		t.Fatalf("exit = %d (out %q err %q)", code, outS, errS)
	}
	// newest first: the failed deploy must print before the reconcile
	if !strings.Contains(outS, failedID) || !strings.Contains(outS, succeededID) {
		t.Fatalf("rows missing:\n%s", outS)
	}
	if strings.Index(outS, failedID) > strings.Index(outS, succeededID) {
		t.Fatalf("history not newest-first:\n%s", outS)
	}
	if !strings.Contains(outS, "topctl logs tops/web") {
		t.Fatalf("failed row lacks the actionable logs hint:\n%s", outS)
	}
	if !strings.Contains(outS, "last known-good") {
		t.Fatalf("failed row lacks the last-known-good fact:\n%s", outS)
	}
}

// TestHistoryJSON: -o json carries the contract row vocabulary and the
// hint for the failed service.
func TestHistoryJSON(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, false)
	seedHistoryOps(t, fx)
	outS, _, code := fx.run(t, "history", "-o", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("not one JSON object: %q", outS)
	}
	ops, _ := env.Data["operations"].([]any)
	if len(ops) != 2 {
		t.Fatalf("operations = %v", env.Data["operations"])
	}
	first := ops[0].(map[string]any)
	if first["state"] != string(domain.OpFailed) || first["error"] == nil {
		t.Fatalf("newest row = %v", first)
	}
	if hint, _ := first["hint"].(string); !strings.Contains(hint, "topctl logs tops/web") {
		t.Fatalf("hint = %v", first["hint"])
	}
}

// TestHistoryServiceFilter: [ns/name] narrows the listing (T-309 wiring:
// matched against the redacted evidence recorded in each row).
func TestHistoryServiceFilter(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, false)
	seedHistoryOps(t, fx)
	outS, _, code := fx.run(t, "history", "tops/web")
	if code != ExitOK {
		t.Fatalf("exit = %d (out %q)", code, outS)
	}
	if strings.Count(outS, "op_") != 1 {
		t.Fatalf("filter did not narrow to one row:\n%s", outS)
	}
	if !strings.Contains(outS, "tops/web") {
		t.Fatalf("kept row lacks the service name:\n%s", outS)
	}
}

// TestHistoryFreshHost: no state store ever existed → an honest empty
// listing, exit 0 (both modes; the store is never invented).
func TestHistoryFreshHost(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, false)
	outS, _, code := fx.run(t, "history")
	if code != ExitOK || !strings.Contains(outS, "no operations recorded yet") {
		t.Fatalf("(out %q, code %d)", outS, code)
	}
	outS, _, code = fx.run(t, "history", "-o", "json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("not one JSON object: %q", outS)
	}
	if ops, _ := env.Data["operations"].([]any); len(ops) != 0 {
		t.Fatalf("operations = %v", env.Data["operations"])
	}
}
