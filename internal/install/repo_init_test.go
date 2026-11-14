// SPDX-License-Identifier: MIT

package install

// T-212 tests: the --init seeding flow (internal/install/repo_init.go) and
// the ZERO-CHANGES guard of the existing-repo path (ADR-0005). The fake
// runner records every call (argv and, through the env-recording shim
// below, the child environment): credential-never-on-argv and the
// seed-verbs-are-the-only-remote-mutating-git-verbs are call-log
// assertions.
//
// Tiers used here: fake gits (golden verb sequence), a REAL local bare
// repo as the git remote (`git init --bare` in a temp dir — the tier-1
// test double for a provider remote, no containers), and full phased
// Install runs over a temp sqlite store.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store"
)

// envRecordRunner wraps a Runner (typically wrappedRunner(f), the flip
// shim of install_test.go around exectest.FakeRunner) and additionally
// records the per-call child environment handed to RunEnv — the seam where
// --init's credential travels.
type envRecordRunner struct {
	inner exec.Runner

	mu    sync.Mutex
	calls []envCallRecord
}

type envCallRecord struct {
	env  []string
	name string
	args []string
}

func (e *envRecordRunner) record(env []string, name string, args ...string) {
	e.mu.Lock()
	e.calls = append(e.calls, envCallRecord{slices.Clone(env), name, slices.Clone(args)})
	e.mu.Unlock()
}

func (e *envRecordRunner) snapshot() []envCallRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]envCallRecord(nil), e.calls...)
}

func (e *envRecordRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	e.record(nil, name, args...)
	return e.inner.Run(ctx, name, args...)
}

// RunEnv records the env, then delegates WITHOUT it (the fake answers by
// argv — exactly what the seed verbs need).
func (e *envRecordRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	e.record(env, name, args...)
	return e.inner.Run(ctx, name, args...)
}

func (e *envRecordRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	e.record(nil, name, args...)
	return e.inner.RunStream(ctx, stdout, stderr, name, args...)
}

// recordArgvOf is the env runner's full call log in "name args" form.
func recordArgvOf(er *envRecordRunner) []string {
	out := []string{}
	for _, c := range er.snapshot() {
		out = append(out, c.name+" "+strings.Join(c.args, " "))
	}
	return out
}

// envOfCall returns the recorded env of the LAST call whose argv contains
// substring.
func envOfCall(er *envRecordRunner, substring string) []string {
	snap := er.snapshot()
	for i := len(snap) - 1; i >= 0; i-- {
		c := snap[i]
		if strings.Contains(strings.Join(append([]string{c.name}, c.args...), " "), substring) {
			return c.env
		}
	}
	return nil
}

// seedWorkdirRe replaces the randomly named seed worktree; dataDir is
// replaced with <datadir> so goldens stay path-stable across runs.
var seedWorkdirRe = regexp.MustCompile(`top-init-[0-9]+`)

func normalizeWorkdirs(dataDir string, lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.ReplaceAll(line, dataDir, "<datadir>")
		out = append(out, seedWorkdirRe.ReplaceAllString(line, "<seed>"))
	}
	return out
}

// gitOnly filters the argv log to git calls.
func gitOnly(er *envRecordRunner) []string {
	var out []string
	for _, line := range recordArgvOf(er) {
		if strings.HasPrefix(line, "git ") {
			out = append(out, line)
		}
	}
	return out
}

// initTestRunner assembles the --init fixture: opts with the flag on and
// deterministic inputs, fake + env recorder, green doctor/snapshot/phase
// seeds, and the requested ls-remote reply (empty for greenfield).
func initTestRunner(t *testing.T, dokkuAbsent bool, lsRemote exectest.Response) (Options, store.Store, *exectest.FakeRunner, *envRecordRunner) {
	t.Helper()
	f := exectest.NewFakeRunner()
	er := &envRecordRunner{inner: wrappedRunner(f)}
	st := testStore(t)
	opts := testOptions(t, er, st, f)
	opts.Init = true
	opts.InitVersion = "test-v0.0.0"
	seedDoctor(f, dokkuAbsent)
	seedSnapshot(f)
	seedPhases(f)
	f.SetPrefix([]string{"git", "ls-remote"}, lsRemote)
	return opts, st, f, er
}

// seedVerbs are the git sub-commands that would ever touch a REMOTE's
// state; zero-changes and refusal guards assert none of them ran.
var seedVerbs = []string{" init ", " add ", " commit ", " push ", " remote "}

func containsSeedVerb(lines []string) string {
	for _, line := range lines {
		for _, v := range seedVerbs {
			if strings.Contains(line, v) || strings.HasSuffix(line, strings.TrimSpace(v)) {
				return line
			}
		}
	}
	return ""
}

// TestInitSeedGoldenVerbSequence: with --init on an EMPTY remote, the git
// verb stream is exactly: two phase-0 ls-remote probes (error probe + data
// probe — checkRepo's shape), one phase-3 fresh probe in ref form, then
// init/add/commit/remote add/-u push. Nothing before, nothing after.
func TestInitSeedGoldenVerbSequence(t *testing.T) {
	t.Parallel()
	opts, _, _, er := initTestRunner(t, false /* dokku present */, respOK("")) // empty remote

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v\nargv:\n%s", err, strings.Join(recordArgvOf(er), "\n"))
	}

	want := []string{
		"git ls-remote " + repoURL + " main",
		"git ls-remote " + repoURL + " main",
		"git ls-remote " + repoURL + " refs/heads/main",
		"git -C <datadir>/git/<seed> init -b main",
		"git -C <datadir>/git/<seed> add -A",
		"git -C <datadir>/git/<seed> -c user.name=TOP platform seeding -c user.email=top@example.invalid -c commit.gpgsign=false commit -m top: platform repo initialized by TOP test-v0.0.0",
		"git -C <datadir>/git/<seed> remote add origin " + repoURL,
		"git -C <datadir>/git/<seed> push -u origin main",
	}
	got := normalizeWorkdirs(opts.DataDir, gitOnly(er))
	if !slices.Equal(got, want) {
		t.Errorf("git verb sequence mismatch:\n got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if !slices.ContainsFunc(res.Created, func(s string) bool {
		return strings.HasPrefix(s, "platform-repo seed commit pushed to ")
	}) {
		t.Errorf("created list must carry the seed push: %v", res.Created)
	}
	if !slices.ContainsFunc(res.Notes, func(s string) bool {
		return strings.Contains(s, "deploy-key FILE PATH") && strings.Contains(s, "never read by TOP")
	}) {
		t.Errorf("notes must carry the ssh credential semantics: %v", res.Notes)
	}

	// Worktree hygiene: no seed temp dir, no askpass shim left behind —
	// only the phase-3 key material lives in <dataDir>/git after the run.
	entries, err := os.ReadDir(filepath.Join(opts.DataDir, "git"))
	if err != nil {
		t.Fatalf("read git work dir: %v", err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"id_ed25519", "known_hosts"}) {
		t.Errorf("leftovers in <dataDir>/git: %v (only id_ed25519/known_hosts expected)", left)
	}
}

// TestExistingRepoZeroRemoteChanges (the ADR-0005 zero-changes guard): a
// plain install over a NON-empty repo must not run a single remote-
// mutating git verb — only the read-only probes.
func TestExistingRepoZeroRemoteChanges(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	er := &envRecordRunner{inner: wrappedRunner(f)}
	st := testStore(t)
	opts := testOptions(t, er, st, f) // no --init: the default contract
	seedDoctor(f, false)
	seedRepo(f)
	seedSnapshot(f)
	seedPhases(f)

	if _, err := Install(context.Background(), opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	if leaked := containsSeedVerb(gitOnly(er)); leaked != "" {
		t.Fatalf("mutating git verb on an existing-repo install: %q", leaked)
	}
	if !slices.Contains(gitOnly(er), "git ls-remote "+repoURL+" "+branch) {
		t.Errorf("expected the read-only ls-remote probes: %v", gitOnly(er))
	}
}

// TestInitRefusedOnForeignContent: --init on a NON-empty remote refuses
// cleanly with the evidence, runs no seed verb, and leaves the content.
func TestInitRefusedOnForeignContent(t *testing.T) {
	t.Parallel()
	opts, _, _, er := initTestRunner(t, false, respOK(lsRemoteOut)) // content present

	_, err := Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), ErrRepoNotEmpty.Error()) ||
		!strings.Contains(err.Error(), "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678") {
		t.Fatalf("refusal must be typed and carry the remote evidence, got: %v", err)
	}
	got := gitOnly(er)
	// Phase 0's two probes plus phase 3's fresh evidence probe (ref form):
	// the refusal happens AFTER the fresh evidence, and nothing mutates.
	if !slices.Equal(got, []string{
		"git ls-remote " + repoURL + " " + branch,
		"git ls-remote " + repoURL + " " + branch,
		"git ls-remote " + repoURL + " refs/heads/" + branch,
	}) {
		t.Errorf("refusal must stop at the three read-only probes, got:\n%s", strings.Join(got, "\n"))
	}
	if leaked := containsSeedVerb(got); leaked != "" {
		t.Fatalf("refused --init mutated the remote: %q", leaked)
	}
}

// TestInitResumesOnOwnSeed: a previous run of THIS install recorded
// initPushed on the newest phase-3 op → a re-run over the now-non-empty
// remote skips the push instead of refusing (the resume case: the seed
// commit landed, a later step failed).
func TestInitResumesOnOwnSeed(t *testing.T) {
	t.Parallel()
	opts, st, _, er := initTestRunner(t, false, respOK(lsRemoteOut)) // content: OUR seed

	op, oerr := domain.NewOperation(domain.OpInstall, domain.ActorUser, "", "")
	if oerr != nil {
		t.Fatal(oerr)
	}
	op.Details = map[string]any{"phase": float64(phaseWiring), "phaseName": phaseNames[phaseWiring], "initPushed": true}
	if err := st.InsertOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	// pending → running → failed (the store refuses pending → failed).
	if err := st.UpdateOperationState(context.Background(), op.ID, domain.OpRunning, "post-push interruption (fixture)", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOperationState(context.Background(), op.ID, domain.OpFailed, "post-push interruption (fixture)", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("resume must skip the re-init, got: %v", err)
	}
	if got := gitOnly(er); len(got) != 3 {
		t.Errorf("resume must not push again (3 probe calls expected):\n%s", strings.Join(got, "\n"))
	}
	if leaked := containsSeedVerb(gitOnly(er)); leaked != "" {
		t.Fatalf("resume must not re-push: %q", leaked)
	}
	if !slices.ContainsFunc(res.Checked, func(s string) bool {
		return strings.Contains(s, "skipping re-init")
	}) {
		t.Errorf("resume must record the skip: %v", res.Checked)
	}
	if slices.ContainsFunc(res.Created, func(s string) bool {
		return strings.Contains(s, "seed commit pushed")
	}) {
		t.Errorf("resume must not claim a fresh seed push: %v", res.Created)
	}
}

// TestInitUnreachableRemoteRefusal: the remote dies between phase 0 (two
// green probes serve both checkRepo reads) and phase 3's fresh evidence
// probe → typed clean refusal; no seed verb ever runs.
func TestInitUnreachableRemoteRefusal(t *testing.T) {
	t.Parallel()
	opts, _, f, er := initTestRunner(t, false, respOK("")) // phase 0 sees empty = --init's precondition
	f.SetScriptedPrefix([]string{"git", "ls-remote"}, []exectest.Response{
		respOK(""), respOK(""), // phase 0: probe + data read
		respExit(128), // phase 3: remote gone
	}, respExit(128))

	_, err := Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), ErrRemoteUnreachable.Error()) {
		t.Fatalf("want typed unreachable refusal, got: %v", err)
	}
	if leaked := containsSeedVerb(gitOnly(er)); leaked != "" {
		t.Fatalf("unreachable refusal must not mutate the remote: %q", leaked)
	}
}

// TestInitTokenByEnvOnlyNeverOnArgv (https remote + one-time token): the
// token reaches ONLY the recorded child env of the probe/push, answered
// through the 0700 askpass shim — never argv, never a stored file
// (deleted afterwards; the shim source references the env, not a literal).
func TestInitTokenByEnvOnlyNeverOnArgv(t *testing.T) {
	t.Parallel()
	const (
		fakeToken = "FAKE-TOKEN-not-a-real-credential-000"
		httpsURL  = "https://git.example.com/example/platform.git"
	)
	opts, _, f, er := initTestRunner(t, false, respOK(""))
	opts.RepoURL = httpsURL
	f.SetOutput("git", respOK(""), "-C") // every seed verb answers green
	opts.InitToken = fakeToken

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v\nargv:\n%s", err, strings.Join(recordArgvOf(er), "\n"))
	}
	_ = res

	snap := er.snapshot()
	for _, rec := range snap {
		recLine := rec.name + " " + strings.Join(rec.args, " ")
		if strings.Contains(recLine, fakeToken) {
			t.Fatalf("token leaked into the argv record: %q", recLine)
		}
	}
	pushEnv := envOfCall(er, "push")
	if !slices.Contains(pushEnv, "TOP_INIT_TOKEN="+fakeToken) {
		t.Fatalf("push env must carry the one-time token: %v", pushEnv)
	}
	var askpassPath string
	for _, kv := range pushEnv {
		if strings.HasPrefix(kv, "GIT_ASKPASS=") {
			askpassPath = strings.TrimPrefix(kv, "GIT_ASKPASS=")
		}
	}
	if askpassPath == "" {
		t.Fatalf("push env missing GIT_ASKPASS: %v", pushEnv)
	}
	if !strings.HasPrefix(askpassPath, filepath.Join(opts.DataDir, "git")) {
		t.Errorf("askpass shim must live inside the TOP data dir (0700): %s", askpassPath)
	}
	// The shim is deleted after the run and its source never contains the
	// token literal — it reads the environment TOP handed to git.
	if _, statErr := os.Stat(askpassPath); !os.IsNotExist(statErr) {
		t.Errorf("askpass shim must be deleted after the push (got: %v)", statErr)
	}
	if s := askpassScript(); !strings.Contains(s, "$TOP_INIT_TOKEN") || strings.Contains(s, fakeToken) {
		t.Errorf("askpass shim must reference the env, never the literal value")
	}
	if !slices.ContainsFunc(res.Notes, func(s string) bool {
		return strings.Contains(s, "one-time credential from the environment") && strings.Contains(s, "never logged")
	}) {
		t.Errorf("notes must carry the token threat model: %v", res.Notes)
	}
}

// TestInitSSHTransportCarriesKeyPathOnly: ssh remotes authenticate the
// push with the deploy-key FILE PATH inside GIT_SSH_COMMAND — exactly
// gitsource.SSHCommand; no token env is ever present there.
func TestInitSSHTransportCarriesKeyPathOnly(t *testing.T) {
	t.Parallel()
	opts, _, _, er := initTestRunner(t, false, respOK(""))

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	_ = res
	wantSSH := "GIT_SSH_COMMAND=" + gitsource.SSHCommand(
		filepath.Join(opts.DataDir, "git", "id_ed25519"),
		filepath.Join(opts.DataDir, "git", "known_hosts"))
	pushEnv := envOfCall(er, "push")
	if !slices.Contains(pushEnv, wantSSH) {
		t.Errorf("ssh push env must carry the strict GIT_SSH_COMMAND with the key PATH:\nwant %s\ngot  %v", wantSSH, pushEnv)
	}
	for _, kv := range pushEnv {
		if strings.HasPrefix(kv, "TOP_INIT_TOKEN=") {
			t.Errorf("ssh transport must never carry a token env: %q", kv)
		}
	}
}

// hybridRunner routes git to a REAL exec (the local bare remote double)
// and everything else to the fake — install phase bodies stay faked while
// the git seeding actually happens against a real repository.
type hybridRunner struct {
	fake *exectest.FakeRunner
}

var (
	_ exec.Runner = hybridRunner{}
	_ envRunner   = hybridRunner{}
)

func (h hybridRunner) isGit(name string) bool { return name == "git" }

func (h hybridRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if h.isGit(name) {
		return realExecRun(ctx, nil, name, args...)
	}
	// Non-git verbs go through the FULL fake seam (flip included): the
	// apps:exists probe must flip green after apps:create exactly like the
	// non-hybrid install tests, or waitHealthy polls a ghost app.
	return wrappedRunner(h.fake).Run(ctx, name, args...)
}

func (h hybridRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	if h.isGit(name) {
		return realExecRun(ctx, env, name, args...)
	}
	_, _, err := wrappedRunner(h.fake).Run(ctx, name, args...) // env dropped: only git carries it
	return nil, nil, err
}

func (h hybridRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	if h.isGit(name) {
		_, _, err := realExecRun(ctx, nil, name, args...)
		return err
	}
	return h.fake.RunStream(ctx, stdout, stderr, name, args...)
}

// realExecRun clones a fresh FastExec per call (the gitsource.FastRunner
// race-free discipline) so ExtraEnv is per-call only.
func realExecRun(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	fe := exec.NewFastExec()
	if len(env) > 0 {
		fe.ExtraEnv = env
	}
	return fe.Run(ctx, name, args...)
}

// TestInitIntegrationRealGitRemote: full phased install over temp sqlite
// with a REAL `git init --bare` remote: the seed commit REALLY lands, with
// the deterministic identity and message; the tree is the rendered
// template; the template's spec parses as a strict platform-repo spec.
func TestInitIntegrationRealGitRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	remote := filepath.Join(t.TempDir(), "platform.git")
	real := exec.NewFastExec()
	if _, stderr, err := real.Run(ctx, "git", "init", "--bare", "-b", "main", remote); err != nil {
		t.Fatalf("git init --bare: %v (%s)", err, stderr)
	}

	f := exectest.NewFakeRunner()
	hybrid := hybridRunner{fake: f} // git → real; dokku/docker/ss → fake
	st := testStore(t)
	opts := testOptions(t, hybrid, st, f)
	opts.Init = true
	opts.InitVersion = "test-v0.0.0"
	opts.RepoURL = "file://" + remote // the empty bare repo is the remote
	seedDoctor(f, false)
	seedSnapshot(f)
	seedPhases(f) // no ls-remote seeding: git runs against the real remote

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	gitDir := []string{"--git-dir", remote}
	out, stderr, err := real.Run(ctx, "git", append(gitDir, "log", "--format=%an|%ae|%s", "-n", "1")...)
	if err != nil {
		t.Fatalf("read seed commit: %v (%s)", err, stderr)
	}
	wantCommit := "TOP platform seeding|top@example.invalid|top: platform repo initialized by TOP test-v0.0.0"
	if strings.TrimSpace(string(out)) != wantCommit {
		t.Errorf("seed commit = %q, want %q", strings.TrimSpace(string(out)), wantCommit)
	}

	out, _, err = real.Run(ctx, "git", append(gitDir, "ls-tree", "-r", "main", "--name-only")...)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	paths := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, want := range []string{"README.md", "core/top-controller.yaml", "observability/prometheus.yaml"} {
		if !slices.Contains(paths, want) {
			t.Errorf("seed tree missing %q: %v", want, paths)
		}
	}

	out, _, err = real.Run(ctx, "git", append(gitDir, "cat-file", "blob", "main:core/top-controller.yaml")...)
	if err != nil {
		t.Fatalf("cat-file: %v", err)
	}
	if !strings.Contains(string(out), opts.ImageRef()) {
		t.Errorf("committed spec must render the pinned image %q:\n%s", opts.ImageRef(), out)
	}

	// The created list claims exactly one seeded commit.
	if !slices.ContainsFunc(res.Created, func(s string) bool {
		return strings.HasPrefix(s, "platform-repo seed commit pushed to file://"+remote) && strings.Contains(s, "10 file(s)")
	}) {
		t.Errorf("created list must carry the seed push: %v", res.Created)
	}

	// The rendered template parses under the strict ServiceSpec loader
	// (platform-repo mode unlocks the reserved core/observability namespaces).
	tmp := t.TempDir()
	if _, err := renderTemplate(tmp, opts.ImageRef()); err != nil {
		t.Fatalf("render: %v", err)
	}
	sr, lerr := spec.LoadDir(tmp, spec.WithPlatformRepo(true))
	if lerr != nil {
		t.Fatalf("rendered template must validate as specs: %v", lerr)
	}
	names := []string{}
	for _, s := range sr.Services {
		names = append(names, s.Ref.Namespace.String()+"/"+s.Ref.Name.String())
	}
	if !slices.Contains(names, "core/top-controller") {
		t.Errorf("template must seed core/top-controller, loaded: %v", names)
	}
}

// TestTemplateRenderIsIdempotent: same inputs → byte-identical trees, and
// the placeholder is fully substituted.
func TestTemplateRenderIsIdempotent(t *testing.T) {
	t.Parallel()
	a, b := t.TempDir(), t.TempDir()
	n1, err := renderTemplate(a, "ghcr.io/toledompm/top/topctl:v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	n2, err := renderTemplate(b, "ghcr.io/toledompm/top/topctl:v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if n1 != n2 || n1 != 10 {
		t.Errorf("rendered file count: %d vs %d (want 10: README + 5 specs + prometheus config + 3 grafana provisioning files)", n1, n2)
	}
	trees := []map[string]string{}
	for _, dir := range []string{a, b} {
		m := map[string]string{}
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				content, _ := os.ReadFile(p)
				m[strings.TrimPrefix(p, dir)] = string(content)
			}
			return nil
		})
		trees = append(trees, m)
	}
	if !slices.Equal(mapKeysSorted(trees[0]), mapKeysSorted(trees[1])) {
		t.Fatalf("rendered trees differ: %v vs %v", mapKeysSorted(trees[0]), mapKeysSorted(trees[1]))
	}
	for k := range trees[0] {
		if trees[0][k] != trees[1][k] {
			t.Errorf("render content differs for %s", k)
		}
	}
	controllerYaml := trees[0]["/core/top-controller.yaml"]
	if strings.Contains(controllerYaml, controllerImageToken) {
		t.Errorf("placeholder survived the render")
	}
}

// mapKeysSorted is the deterministic key list for byte-tree comparison.
func mapKeysSorted(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestInitDryRunPlansTheSeedAndMutatesNothing: --init under dry-run prints
// the plan clause and pushes absolutely nothing.
func TestInitDryRunPlansTheSeedAndMutatesNothing(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	er := &envRecordRunner{inner: wrappedRunner(f)}
	st := testStore(t)
	opts := testOptions(t, er, st, f)
	opts.Init = true
	opts.DryRun = true
	opts.Confirm = false // dry-run needs no approval (nothing mutates)
	seedDoctor(f, true)

	res, err := Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("dry-run with --init: %v", err)
	}
	found := false
	for _, p := range res.PhasesRun {
		found = found || strings.Contains(p, "--init")
	}
	if !found {
		t.Errorf("dry-run plan must mention the --init clause: %v", res.PhasesRun)
	}
	if leaked := containsSeedVerb(gitOnly(er)); leaked != "" {
		t.Fatalf("dry-run must never mutate the remote: %q", leaked)
	}
}
