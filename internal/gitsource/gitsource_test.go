// SPDX-License-Identifier: MIT

package gitsource

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
)

// ---------------------------------------------------------------------------
// test double
// ---------------------------------------------------------------------------

// envFake wraps exectest.FakeRunner to satisfy gitsource's env-carrying
// Runner, RECORDING the env of every call so tests can assert on
// GIT_SSH_COMMAND — and that neither argv nor env ever carries key CONTENT.
type envFake struct {
	inner *exectest.FakeRunner
	mu    sync.Mutex
	envs  [][]string
	// realFor lists command names that pass through un-faked (argv, env
	// and all) into a real exec.FastExec. Used for `tar` here: the fake
	// cannot actually unpack, and archive extraction is exactly what
	// tier-1 must prove. `git` stays faked.
	realFor map[string]bool
}

func newEnvFake() *envFake {
	return &envFake{
		inner:   exectest.NewFakeRunner(),
		realFor: map[string]bool{"tar": true},
	}
}

func (f *envFake) Run(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error) {
	f.record(env)
	if f.realFor[name] {
		return realRunner().Run(ctx, name, args...)
	}
	return f.inner.Run(ctx, name, args...)
}

// realRunner is a fresh exec.FastExec per call (never shared state).
func realRunner() *exec.FastExec { return &exec.FastExec{WaitDelay: 2 * time.Second} }

func (f *envFake) RunStream(ctx context.Context, env []string, stdout, stderr io.Writer, name string, args ...string) error {
	f.record(env)
	if f.realFor[name] {
		return realRunner().RunStream(ctx, stdout, stderr, name, args...)
	}
	return f.inner.RunStream(ctx, stdout, stderr, name, args...)
}

func (f *envFake) record(env []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envs = append(f.envs, append([]string{}, env...))
}

func (f *envFake) allEnvs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string{}, f.envs...)
}

func (f *envFake) cmds() []exectest.Call { return f.inner.Recorded() }

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const (
	testURL    = "git@github.com:example/top-platform.git" // fictional per repo rules
	testBranch = "main"
	testSHA    = "0123456789abcdef0123456789abcdef01234567"
	// secretMaterial plays key CONTENTS: never read, must never appear in
	// argv or env anywhere.
	secretMaterial = "KEY-MATERIAL-TOKEN-42"
)

func testSource(t *testing.T) (*Source, *envFake) {
	t.Helper()
	f := newEnvFake()
	s := &Source{
		URL:        testURL,
		Branch:     testBranch,
		KeyPath:    filepath.Join(t.TempDir(), "git", "id_ed25519"),
		KnownHosts: filepath.Join(t.TempDir(), "git", "known_hosts"),
		WorkDir:    filepath.Join(t.TempDir(), "git"),
		Runner:     f,
	}
	return s, f
}

// seedEnv writes plausible key/pinned-host files (path-referenced only).
func seedEnv(t *testing.T) (keyPath, knownHosts string) {
	t.Helper()
	base := t.TempDir()
	keyPath = filepath.Join(base, "id_ed25519")
	knownHosts = filepath.Join(base, "known_hosts")
	if err := os.WriteFile(keyPath, []byte(secretMaterial), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHosts, []byte("github.com ssh-ed25519 AAAATestKey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath, knownHosts
}

// seedMirror fakes a completed mirror clone: HEAD marker on disk.
func seedMirror(t *testing.T, s *Source) string {
	t.Helper()
	mirror := s.MirrorDir()
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mirror
}

// seedShas wires ls-remote to return successive shas per call (scripted):
// call n gets shas[n], later calls repeat the last one.
func seedShas(f *envFake, shas ...string) {
	script := make([]exectest.Response, len(shas))
	for i, sha := range shas {
		script[i] = exectest.Response{
			Stdout: []byte(sha + "\trefs/heads/" + testBranch + "\n"),
		}
	}
	f.inner.SetScriptedPrefix([]string{"git", "ls-remote"}, script, script[len(script)-1])
}

// tarBytes hand-builds a tiny tar (stdlib archive/tar) for the fake
// `git archive` stdout.
func tarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// scriptedFetch wires the full happy-path script: no clone needed (mirror
// pre-seeded) → fetch → archive(tarBytes(files)) with a permanent success
// tail; tar is separately seeded success.
func scriptedFetch(t *testing.T, f *envFake, files map[string]string) {
	f.inner.SetScriptedPrefix([]string{"git"}, []exectest.Response{
		{},                           // fetch
		{Stdout: tarBytes(t, files)}, // archive
	}, exectest.Response{})
	f.inner.SetPrefix([]string{"tar"}, exectest.Response{})
}

// ---------------------------------------------------------------------------
// construction / config mapping
// ---------------------------------------------------------------------------

func TestNewFromConfigLayout(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.DataDir = "/var/lib/top"
	cfg.Git.URL = testURL
	s := New(cfg)
	if s.URL != testURL || s.Branch != "main" {
		t.Fatalf("URL/branch not mapped: %+v", s)
	}
	checks := []struct{ label, got, want string }{
		{"keyPath", s.KeyPath, "/var/lib/top/git/id_ed25519"},
		{"knownHosts", s.KnownHosts, "/var/lib/top/git/known_hosts"},
		{"workDir", s.WorkDir, "/var/lib/top/git"},
		{"mirrorDir", s.MirrorDir(), "/var/lib/top/git/mirror.git"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %s want %s", c.label, c.got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// env construction
// ---------------------------------------------------------------------------

func TestSSHCommandGolden(t *testing.T) {
	t.Parallel()
	got := SSHCommand("/var/lib/top/git/id_ed25519", "/var/lib/top/git/known_hosts")
	want := "ssh -i /var/lib/top/git/id_ed25519" +
		" -o IdentitiesOnly=yes" +
		" -o UserKnownHostsFile=/var/lib/top/git/known_hosts" +
		" -o StrictHostKeyChecking=yes"
	if got != want {
		t.Errorf("SSHCommand:\n got: %s\nwant: %s", got, want)
	}
}

func TestSSHEnvGolden(t *testing.T) {
	t.Parallel()
	keyPath, knownHosts := seedEnv(t)

	t.Run("ssh remote gets strict GIT_SSH_COMMAND", func(t *testing.T) {
		s := &Source{URL: testURL, Branch: testBranch, KeyPath: keyPath, KnownHosts: knownHosts}
		env, err := s.env()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"GIT_TERMINAL_PROMPT=0",
			"GIT_SSH_COMMAND=" + SSHCommand(keyPath, knownHosts),
		}
		if !eqSlices(env, want) {
			t.Errorf("env:\n got: %v\nwant: %v", env, want)
		}
	})

	t.Run("ssh remote without known_hosts rejects loudly", func(t *testing.T) {
		s := &Source{URL: testURL, Branch: testBranch, KeyPath: "/run/key"}
		_, err := s.env()
		if err == nil || !strings.Contains(err.Error(), "known_hosts") {
			t.Fatalf("want loud known_hosts refusal; got %v", err)
		}
	})

	t.Run("https remote needs no ssh env", func(t *testing.T) {
		s := &Source{URL: "https://github.com/example/top-platform.git", Branch: testBranch}
		env, err := s.env()
		if err != nil {
			t.Fatal(err)
		}
		if !eqSlices(env, []string{"GIT_TERMINAL_PROMPT=0"}) {
			t.Errorf("https env: %v", env)
		}
	})

	t.Run("plain local path is not ssh", func(t *testing.T) {
		if isSSHRemote("/data/repos/platform.git") {
			t.Error("plain path misclassified as ssh remote")
		}
	})
}

// ---------------------------------------------------------------------------
// golden argv + env carrying
// ---------------------------------------------------------------------------

func TestResolveLatestGolden(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	keyPath, knownHosts := seedEnv(t)
	s.KeyPath, s.KnownHosts = keyPath, knownHosts
	f.inner.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{
		Stdout: []byte(testSHA + "\trefs/heads/main\n"),
	})

	sha, err := s.ResolveLatest(context.Background())
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if sha != testSHA {
		t.Fatalf("sha %q", sha)
	}
	calls := f.cmds()
	if len(calls) != 1 {
		t.Fatalf("exactly one call expected; got %v", calls)
	}
	wantArgs := []string{"git", "ls-remote", testURL, "refs/heads/" + testBranch}
	if !eqCmd(calls[0], wantArgs) {
		t.Errorf("golden argv:\n got: %v\nwant: %v", calls[0], wantArgs)
	}
	assertStrictSSHEnv(t, f, 0)
}

func TestResolveLatestBranchGone(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	f.inner.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{Stdout: nil})
	_, err := s.ResolveLatest(context.Background())
	if !errors.Is(err, ErrRevisionGone) {
		t.Fatalf("absent branch must be ErrRevisionGone; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// error mapping (table)
// ---------------------------------------------------------------------------

type errRow struct {
	name          string
	stderr        string
	wantKind      Kind
	wantSentinel  error
	wantHintWords []string
}

// runResolveFailing drives a failing canned ls-remote with the given
// stderr through the real code path and returns the typed error.
func runResolveFailing(t *testing.T, stderr string) *Error {
	t.Helper()
	s, f := testSource(t)
	f.inner.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{
		Stderr:   []byte(stderr),
		ExitCode: 128,
	})
	_, err := s.ResolveLatest(context.Background())
	var ge *Error
	if !errors.As(err, &ge) {
		t.Fatalf("error not typed (*Error): %v", err)
	}
	return ge
}

func TestErrorMappingTable(t *testing.T) {
	t.Parallel()
	rows := []errRow{
		{
			name: "host-key-verification-failed", stderr: "fatal: Host key verification failed.\n",
			wantKind: KindHostKey, wantSentinel: ErrHostKey, wantHintWords: []string{"known_hosts", "NEVER"},
		},
		{
			name: "host-key-identification-changed", stderr: "@@ REMOTE HOST IDENTIFICATION HAS CHANGED! @@",
			wantKind: KindHostKey, wantSentinel: ErrHostKey,
		},
		{
			name: "permission-denied-publickey", stderr: "ERROR: Permission denied (publickey).",
			wantKind: KindAuth, wantSentinel: ErrGitAuth, wantHintWords: []string{"deploy key"},
		},
		{
			name: "repository-not-found", stderr: "ERROR: Repository not found.",
			wantKind: KindAuth, wantSentinel: ErrGitAuth,
		},
		{
			name: "ref-gone-force-push", stderr: "fatal: couldn't find remote ref " + testSHA,
			wantKind: KindRevisionGone, wantSentinel: ErrRevisionGone, wantHintWords: []string{"force-push"},
		},
		{
			name: "dns-failure", stderr: "ssh: Could not resolve host example.invalid",
			wantKind: KindUnreachable, wantSentinel: ErrGitUnavailable, wantHintWords: []string{"backoff"},
		},
		{
			name: "connection-timed-out", stderr: "ssh: connect to host example.invalid port 22: Operation timed out",
			wantKind: KindUnreachable, wantSentinel: ErrGitUnavailable,
		},
		{
			name: "unknown-128", stderr: "fatal: something entirely novel",
			wantKind: KindInternal, wantSentinel: ErrGitInternal,
		},
		{
			name: "no-stderr-signature", stderr: "",
			wantKind: KindInternal, wantSentinel: ErrGitInternal,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			got := runResolveFailing(t, r.stderr)
			if got.Kind != r.wantKind {
				t.Fatalf("kind: got %v want %v (err: %v)", got.Kind, r.wantKind, got)
			}
			if !errors.Is(got, r.wantSentinel) {
				t.Errorf("sentinel: got %v want %#v", got, r.wantSentinel)
			}
			if r.stderr != "" && !strings.Contains(got.Stderr, firstLine([]byte(r.stderr))) {
				t.Errorf("remote signature excerpt lost: %q", got.Stderr)
			}
			for _, frag := range r.wantHintWords {
				if !strings.Contains(got.Hint, frag) {
					t.Errorf("remediation hint missing %q in: %s", frag, got.Hint)
				}
			}
		})
	}
}

func TestContextAndMissingBinaryMapping(t *testing.T) {
	t.Parallel()
	t.Run("context cancel passes through typed", func(t *testing.T) {
		s, f := testSource(t)
		f.inner.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{
			Err: fmt.Errorf("cancelled: %w", context.Canceled),
		})
		_, err := s.ResolveLatest(context.Background())
		var ge *Error
		if !errors.As(err, &ge) || ge.Kind != KindInternal {
			t.Fatalf("cancelled should stay internal-typed: %v", err)
		}
	})
	t.Run("binary missing maps with hint", func(t *testing.T) {
		s, f := testSource(t)
		f.inner.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{
			Err: fmt.Errorf("run git: %w: no such binary", exec.ErrNotFound),
		})
		_, err := s.ResolveLatest(context.Background())
		var ge *Error
		if !errors.As(err, &ge) || ge.Kind != KindInternal {
			t.Fatalf("missing binary: %v", err)
		}
		if !strings.Contains(ge.Hint, "missing") {
			t.Errorf("hint: %s", ge.Hint)
		}
	})
}

// ---------------------------------------------------------------------------
// mirror lifecycle
// ---------------------------------------------------------------------------

func TestEnsureMirrorClonesOnceGolden(t *testing.T) {
	t.Parallel()
	keyPath, knownHosts := seedEnv(t)
	dir := filepath.Join(t.TempDir(), "work")
	s := &Source{URL: testURL, Branch: testBranch, KeyPath: keyPath, KnownHosts: knownHosts, WorkDir: dir}
	f := newEnvFake()
	s.Runner = f
	f.inner.SetPrefix([]string{"git", "clone"}, exectest.Response{})

	if err := s.EnsureMirror(context.Background()); err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	// Simulate the completed clone: only the fake ran, no real git.
	seedMirror(t, s)
	if err := s.EnsureMirror(context.Background()); err != nil {
		t.Fatalf("EnsureMirror #2: %v", err)
	}
	if err := s.EnsureMirror(context.Background()); err != nil {
		t.Fatalf("EnsureMirror #3: %v", err)
	}

	calls := f.cmds()
	if len(calls) != 1 {
		t.Fatalf("clone must run exactly once; ran %d: %v", len(calls), calls)
	}
	want := []string{"git", "clone", "--mirror", testURL, dir + "/mirror.git"}
	if !eqCmd(calls[0], want) {
		t.Errorf("golden clone argv:\n got: %v\nwant: %v", calls[0], want)
	}
	assertStrictSSHEnv(t, f, 0)
}

func TestEnsureMirrorAbsentKnownHostsFailsBeforeAnyCommand(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	s.KnownHosts = "" // ssh remote, no pin → refuse
	if err := s.EnsureMirror(context.Background()); err == nil {
		t.Fatal("expected refusal")
	}
	if got := f.cmds(); len(got) != 0 {
		t.Fatalf("refusal must prevent exec; ran %v", got)
	}
}

// ---------------------------------------------------------------------------
// fetch + materialize
// ---------------------------------------------------------------------------

func TestFetchRevisionGolden(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	seedMirror(t, s)
	files := map[string]string{
		"specs/core-platform.yaml": "namespace: core\nname: platform\n",
		"README.md":                "specs live here",
	}
	scriptedFetch(t, f, files)

	snap, err := s.FetchRevision(context.Background(), testSHA)
	if err != nil {
		t.Fatalf("FetchRevision: %v", err)
	}
	t.Cleanup(func() { _ = snap.Close() })

	if snap.SHA != testSHA {
		t.Errorf("snapshot sha %q", snap.SHA)
	}
	body, err := os.ReadFile(filepath.Join(snap.Dir, "specs", "core-platform.yaml"))
	if err != nil || !strings.Contains(string(body), "namespace: core") {
		t.Errorf("extracted content wrong: %q err %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(snap.Dir, "README.md")); err != nil {
		t.Errorf("second file missing: %v", err)
	}

	calls := f.cmds()
	want := [][]string{
		{"git", "-C", s.MirrorDir(), "fetch", "--quiet", "origin", testSHA},
		{"git", "-C", s.MirrorDir(), "archive", "--format=tar", testSHA},
	}
	if len(calls) != 2 { // fetch, archive (tar runs for real on the host)
		t.Fatalf("want 2 recorded git calls (fetch/archive); got %d: %v", len(calls), calls)
	}
	for i, w := range want {
		if !eqCmd(calls[i], w) {
			t.Errorf("call %d:\n got: %v\nwant: %v", i, calls[i], w)
		}
	}

	if !envIndexHasSSH(f, 0) || !envIndexHasSSH(f, 1) {
		t.Error("fetch/archive calls must carry strict GIT_SSH_COMMAND env")
	}

	// perms and hygiene: dir 0700, no tarball leftover inside snapshot dir.
	st, err := os.Stat(snap.Dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("snapshot dir stat: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o700 {
		t.Errorf("snapshot dir perms: %o, want 700", mode)
	}
	entries, err := os.ReadDir(snap.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "top-archive-") {
			t.Errorf("temp tarball leaked into snapshot dir: %s", e.Name())
		}
	}
}

func TestFetchRevisionRejectsNonShas(t *testing.T) {
	t.Parallel()
	bad := []string{
		"main", "", "refs/heads/main", "short",
		testSHA + ";rm", string([]byte{0x01}),
		strings.ToUpper(testSHA), // lowercase-only: golden argv stability
	}
	for _, b := range bad {
		s, f := testSource(t)
		_, err := s.FetchRevision(context.Background(), b)
		if !errors.Is(err, ErrInvalidSHA) {
			t.Errorf("%q: want ErrInvalidSHA; got %v", b, err)
		}
		if got := f.cmds(); len(got) != 0 {
			t.Errorf("%q: invalid sha must not exec anything; ran %v", b, got)
		}
	}
	// Valid 64-hex (sha256 repos) accepted at validation level.
	s, f := testSource(t)
	seedMirror(t, s)
	f.inner.SetScriptedPrefix([]string{"git"}, []exectest.Response{{}, {Stdout: tarBytes(t, map[string]string{"x.yaml": ""})}}, exectest.Response{})
	f.inner.SetPrefix([]string{"tar"}, exectest.Response{})
	sha64 := strings.Repeat("ab", 32)
	if _, err := s.FetchRevision(context.Background(), sha64); err != nil {
		t.Errorf("64-hex sha rejected: %v", err)
	}
}

// The M3 acceptance shape: sha vanished (force-push) → loud failure, and
// NOTHING beyond the fetch attempt runs or materializes.
func TestFetchRevisionForcePushNoMutation(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	seedMirror(t, s)
	f.inner.SetScriptedPrefix([]string{"git"}, []exectest.Response{
		{ExitCode: 128, Stderr: []byte("fatal: couldn't find remote ref " + testSHA)},
	}, exectest.Response{})

	_, err := s.FetchRevision(context.Background(), testSHA)
	var ge *Error
	if !errors.As(err, &ge) || ge.Kind != KindRevisionGone || !errors.Is(err, ErrRevisionGone) {
		t.Fatalf("want typed ErrRevisionGone; got %#v", err)
	}
	if got := f.cmds(); len(got) != 1 {
		t.Fatalf("no mutation after failed fetch; ran %d calls: %v", len(got), got)
	}
	if _, err := os.Stat(s.WorkDir); err == nil {
		entries, rdErr := os.ReadDir(s.WorkDir)
		if rdErr == nil {
			for _, d := range entries {
				if strings.Contains(d.Name(), "top-extract-") {
					t.Errorf("materialize dir left behind after failed fetch: %s", d.Name())
				}
			}
		}
	}
}

func TestFetchRevisionWithoutMirrorClonesFirst(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	scriptedFetch(t, f, map[string]string{"a.yaml": "a: 1\ny: 2\n"})
	// No seedMirror: FetchRevision must clone, then fetch, then materialize.
	snap, err := s.FetchRevision(context.Background(), testSHA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	calls := f.cmds()
	if len(calls) != 3 { // clone, fetch, archive (tar runs for real)
		t.Fatalf("want clone+fetch+archive; got %v", calls)
	}
	if !eqCmd(calls[0], []string{"git", "clone", "--mirror", testURL, s.MirrorDir()}) {
		t.Errorf("first call not the golden clone: %v", calls[0])
	}
}

// ---------------------------------------------------------------------------
// snapshot lifecycle
// ---------------------------------------------------------------------------

func TestSnapshotAutoCloseOnContextDone(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	seedMirror(t, s)
	scriptedFetch(t, f, map[string]string{"a.yaml": "a: 1\nb: 2\n"})

	ctx, cancel := context.WithCancel(context.Background())
	snap, err := s.FetchRevision(ctx, testSHA)
	if err != nil {
		t.Fatal(err)
	}
	dir := snap.Dir
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("snapshot dir should exist: %v", err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, statErr := os.Stat(dir)
		if os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot dir not auto-closed after ctx cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := snap.Close(); err != nil {
		t.Errorf("post-cancel Close must be a quiet no-op; got %v", err)
	}
}

func TestSnapshotCloseRemovesExactlyOnce(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	seedMirror(t, s)
	scriptedFetch(t, f, map[string]string{"a.yaml": ""})
	snap, err := s.FetchRevision(context.Background(), testSHA)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := snap.Close(); err != nil {
			t.Fatalf("close #%d: %v", i, err)
		}
	}
	if _, err := os.Stat(snap.Dir); !os.IsNotExist(err) {
		t.Fatalf("dir not removed: %v", err)
	}
}

// materialize failure cleans up its temp dir.
func TestMaterializeFailureCleansTempDir(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	seedMirror(t, s)
	f.inner.SetScriptedPrefix([]string{"git"}, []exectest.Response{
		{}, // fetch ok
		{ExitCode: 128, Stderr: []byte("fatal: bad object")}, // archive fails
	}, exectest.Response{})

	if _, err := s.FetchRevision(context.Background(), testSHA); err == nil {
		t.Fatal("expected archive failure to surface")
	}
	// The 0700 work dir may exist; no extract dirs or tar files may leak.
	entries, err := os.ReadDir(s.WorkDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "top-extract-") || strings.HasPrefix(e.Name(), "top-archive-") {
			t.Errorf("temp leftovers after failed materialize: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// secret posture: nothing content-bearing in argv or env
// ---------------------------------------------------------------------------

func TestNeverCarriesKeyContents(t *testing.T) {
	t.Parallel()
	keyPath, knownHosts := seedEnv(t)
	s, f := testSource(t)
	s.KeyPath, s.KnownHosts = keyPath, knownHosts
	f.inner.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{
		Stdout: []byte(testSHA + "\trefs/heads/main\n"),
	})
	if _, err := s.ResolveLatest(context.Background()); err != nil {
		t.Fatal(err)
	}

	// argv: name references only; no secret material.
	for i, call := range f.cmds() {
		for _, a := range call.Args {
			if strings.Contains(a, secretMaterial) {
				t.Errorf("argv call %d contributes secret content: %q", i, a)
			}
		}
	}
	// env: GIT_SSH_COMMAND present is good and must reference the key by
	// PATH; no env var may carry key CONTENT.
	for i, env := range f.allEnvs() {
		for _, kv := range env {
			if strings.Contains(kv, secretMaterial) {
				t.Errorf("env call %d carries secret content: %q", i, kv)
			}
		}
	}
	assertStrictSSHEnv(t, f, 0)
	env := f.allEnvs()[0]
	var ssh string
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") {
			ssh = kv
		}
	}
	if !strings.Contains(ssh, keyPath) {
		t.Errorf("GIT_SSH_COMMAND must reference the key by path: %q", ssh)
	}
}

// ---------------------------------------------------------------------------
// poller
// ---------------------------------------------------------------------------

func TestPollerPoll(t *testing.T) {
	t.Parallel()
	s, f := testSource(t)
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	seedShas(f, shaA, shaA, shaB)

	p := s.NewPoller(time.Second)
	sha, changed, err := p.Poll(context.Background(), "")
	if err != nil || !changed || sha != shaA {
		t.Fatalf("first poll: sha=%s changed=%v err=%v", sha, changed, err)
	}
	sha, changed, err = p.Poll(context.Background(), shaA)
	if err != nil || changed || sha != shaA {
		t.Fatalf("unchanged poll: sha=%s changed=%v err=%v", sha, changed, err)
	}
	sha, changed, err = p.Poll(context.Background(), shaA)
	if err != nil || !changed || sha != shaB {
		t.Fatalf("changed poll: sha=%s changed=%v err=%v", sha, changed, err)
	}
}

func TestPollerLoopReportsChangeAndSurvivesErrors(t *testing.T) {
	t.Parallel()
	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)

	t.Run("loop reports distinct shas only", func(t *testing.T) {
		s, f := testSource(t)
		seedShas(f, shaA, shaA, shaB, shaA)
		p := s.NewPoller(time.Second)

		seen := make(chan string, 8)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.Loop(ctx, "", func(_ context.Context, sha string) error {
				seen <- sha
				return nil
			})
			close(done)
		}()
		defer func() { cancel(); <-done }()

		var got []string
		deadline := time.Now().Add(5 * time.Second)
		for len(got) < 2 && time.Now().Before(deadline) {
			select {
			case sha := <-seen:
				got = append(got, sha)
			case <-time.After(time.Until(deadline)):
			}
		}
		if len(got) != 2 || got[0] != shaA || got[1] != shaB {
			t.Fatalf("loop reported %v, want [%s %s]", got, shaA, shaB)
		}
	})

	t.Run("loop keeps polling through failures", func(t *testing.T) {
		s, f := testSource(t)
		f.inner.SetScriptedPrefix([]string{"git", "ls-remote"}, []exectest.Response{
			{ExitCode: 128, Stderr: []byte("ssh: Could not resolve host example.invalid")},
		}, exectest.Response{
			Stdout: []byte(shaA + "\trefs/heads/main\n"),
		})
		p := s.NewPoller(time.Second)
		pollErrs := make(chan error, 8)
		p.OnError = func(err error) { pollErrs <- err }
		changes := make(chan string, 8)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go p.Loop(ctx, "", func(_ context.Context, sha string) error {
			changes <- sha
			return nil
		})

		select {
		case err := <-pollErrs:
			var ge *Error
			if !errors.As(err, &ge) || ge.Kind != KindUnreachable {
				t.Fatalf("poll failure should be typed unreachable; got %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("OnError never fired")
		}
		// Recovery: the scripted tail resolves and the loop reports the
		// change — proof the loop did not die on the failure.
		select {
		case sha := <-changes:
			if sha != shaA {
				t.Fatalf("recovery change sha %s, want %s", sha, shaA)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("loop did not recover after failure")
		}
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func assertStrictSSHEnv(t *testing.T, f *envFake, callIdx int) {
	t.Helper()
	envs := f.allEnvs()
	if callIdx >= len(envs) {
		t.Fatalf("no env recorded for call %d (have %d)", callIdx, len(envs))
	}
	joined := strings.Join(envs[callIdx], "\n")
	for _, w := range []string{
		"IdentitiesOnly=yes", "UserKnownHostsFile=", "StrictHostKeyChecking=yes", "GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, w) {
			t.Errorf("env %#v missing %q", envs[callIdx], w)
		}
	}
}

func envIndexHasSSH(f *envFake, idx int) bool {
	envs := f.allEnvs()
	if idx >= len(envs) {
		return false
	}
	for _, kv := range envs[idx] {
		if strings.HasPrefix(kv, "GIT_SSH_COMMAND=") && strings.Contains(kv, "StrictHostKeyChecking=yes") {
			return true
		}
	}
	return false
}

func eqCmd(c exectest.Call, want []string) bool {
	if c.Name != want[0] || len(c.Args) != len(want)-1 {
		return false
	}
	return eqSlices(c.Args, want[1:])
}

func eqSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
