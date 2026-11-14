// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	stdexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// T-205..T-208: the install/uninstall command bodies are real. These are
// the CLI-level wiring tests — envelope shape, exit mapping, and the
// honest full-run path — all against exectest's FakeRunner and temp
// dirs/configs (never the host: no /var/lib/top, no real dokku).
//
// Fine-grained behavior lives in internal/install's tests; this file pins
// the cli composition (config → options → body → envelope/exit code).

// runCLIOpts is runCLI with a Runner seam (the fake) — the only helper here.
func runCLIOpts(t *testing.T, opts Options, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut strings.Builder
	o := opts
	if o.Out == nil {
		o.Out = &out
	}
	if o.Err == nil {
		o.Err = &errOut
	}
	if o.StdinIsTTY == nil {
		tty := false
		o.StdinIsTTY = &tty
	}
	if o.Version == "" {
		o.Version = "test-v0.0.0"
	}
	code := Execute(o, args)
	return out.String(), errOut.String(), code
}

// writeTestConfig renders a minimal top-config.yaml with a temp dataDir
// (the doctor/store/test paths all resolve under it).
func writeTestConfig(t *testing.T, dataDir string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "top-config.yaml")
	yaml := "apiVersion: top/v1alpha1\ndataDir: " + dataDir + "\n" +
		"git:\n  url: ssh://git@git.example.com/example/platform.git\n  branch: main\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedFake runs the fake seeding for a full install: green preflight
// probes, green repo, foreign-only snapshot, green phase verbs.
func seedFake(t *testing.T, f *exectest.FakeRunner) exectest.Response {
	t.Helper()
	f.SetPrefix([]string{"dokku", "version"}, exectest.Response{Stdout: []byte("dokku version 0.38.32 (git commit a11d15d)")})
	f.SetPrefix([]string{"dokku", "apps:list"}, exectest.Response{Stdout: []byte("[]")})
	f.SetPrefix([]string{"docker", "version"}, exectest.Response{Stdout: []byte("27.0.3")})
	f.SetPrefix([]string{"sudo"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"git", "ls-remote"}, exectest.Response{Stdout: []byte("a1b2c3\trefs/heads/main")})
	f.SetPrefix([]string{"docker", "ps"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"docker", "network"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"docker", "volume"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"ss"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"dokku", "network:exists"}, exectest.Response{ExitCode: 1})
	f.SetPrefix([]string{"dokku", "network:create"}, exectest.Response{Stdout: []byte("1")})
	f.SetPrefix([]string{"dokku", "storage:info"}, exectest.Response{ExitCode: 1})
	f.SetPrefix([]string{"dokku", "storage:create"}, exectest.Response{Stdout: []byte("1")})
	f.SetScriptedPrefix([]string{"dokku", "apps:exists"}, []exectest.Response{{ExitCode: 20}}, exectest.Response{})
	f.SetPrefix([]string{"dokku", "apps:create"}, exectest.Response{Stdout: []byte("1")})
	f.SetPrefix([]string{"dokku", "storage:mount"}, exectest.Response{Stdout: []byte("1")})
	f.SetPrefix([]string{"dokku", "git:from-image"}, exectest.Response{Stdout: []byte("1")})
	f.SetPrefix([]string{"dokku", "ps:report"}, exectest.Response{Stdout: []byte(`{"app":"core-top-controller","deployed":"true","running":"true"}`)})
	f.SetPrefix([]string{"dokku", "builds:report"}, exectest.Response{Stdout: []byte(`{"build-status":"succeeded"}`)})
	f.SetPrefix([]string{"dokku", "git:report"}, exectest.Response{Stdout: []byte(`{"source-image":"ghcr.io/toledompm/top/topctl:v0.1.0"}`)})
	f.SetPrefix([]string{"dokku", "apps:report"}, exectest.Response{Stdout: []byte(`{"deploy-source":"docker-image","deploy-source-metadata":"ghc"}`)})
	f.SetPrefix([]string{"dokku", "network:report"}, exectest.Response{Stdout: []byte(`{"computed-attach-post-create":"top-internal"}`)})
	f.SetPrefix([]string{"dokku", "resource:report"}, exectest.Response{Stdout: []byte(`{}`)})
	f.SetPrefix([]string{"dokku", "ports:report"}, exectest.Response{Stdout: []byte(`[]`)})
	f.SetPrefix([]string{"dokku", "storage:list"}, exectest.Response{Stdout: []byte(`[]`)})
	f.SetPrefix([]string{"dokku", "config:keys"}, exectest.Response{Stdout: []byte("TZ\n")})
	return exectest.Response{}
}

// need a key fixture: the v0 installer requires --deploy-key/--known-hosts
// paths; the happy-path run points both at fixture files.
func writeKeyFixtures(t *testing.T, dir string) (string, string) {
	t.Helper()
	key := filepath.Join(dir, "deploy_key")
	if err := os.WriteFile(key, []byte("FAKEKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte("git.example.com ssh-ed25519 AAAAFAKE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, kh
}

// seedInstalledStore creates a migrated sqlite store at <dataDir>/top.db
// (the file the CLI's uninstall discovers) with one bootstrap-owned app
// row fixture.
func seedInstalledStore(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(context.Background(), filepath.Join(dataDir, "top.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InsertLedgerEntry(context.Background(), &store.Resource{
		Kind:        "app",
		Identifier:  "core-top-controller",
		Labels:      map[string]string{"top.dev/managed": "true", "top.dev/namespace": "core", "top.dev/service": "top-controller"},
		CreatedByOp: "fixture-op-install",
		State:       store.ResourceBootstrapOwned,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// splitGitRunner routes git/tar to the REAL host binaries and everything
// else (dokku, wget, sudo probes) to the fake — the install split-runner
// pattern, used by the CLI-level first-reconciliation wiring tests where
// gitsource must run for real over a file:// fixture repo.
type splitGitRunner struct{ f *exectest.FakeRunner }

func (s splitGitRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" || name == "tar" {
		var stderr bytes.Buffer
		out, err := stdexec.CommandContext(ctx, name, args...).Output() //nolint:gosec // test fixture, project-local repo
		return out, stderr.Bytes(), err
	}
	return s.f.Run(ctx, name, args...)
}

func (s splitGitRunner) RunStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	if name == "git" || name == "tar" {
		c := stdexec.CommandContext(ctx, name, args...)
		c.Stdout, c.Stderr = stdout, stderr //nolint:gosec // test fixture, project-local repo
		return c.Run()
	}
	return s.f.RunStream(ctx, stdout, stderr, name, args...)
}

// RunEnv satisfies install's envRunner seam (the phase-0 repo probe). The
// test's remote is file:// — the probe env is empty (sshProbeEnv contract)
// and git/tar run for real without it; dokku goes to the fake.
func (s splitGitRunner) RunEnv(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	if len(env) != 0 {
		return nil, nil, fmt.Errorf("splitGitRunner: unexpected non-empty probe env %v (file:// remotes probe env-less)", env)
	}
	return s.Run(ctx, name, args...)
}

// seedRepoFixture creates a minimal local platform repo (controller spec
// matching the bootstrap deployment's image → the adoption's noop plan)
// and commits it; returns the file:// URL and HEAD sha.
func seedRepoFixture(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir() + "/platform-repo"
	if err := fixtureRepo(t, repo, map[string]string{"core/top-controller.yaml": specControllerFix}); err != nil {
		t.Fatal(err)
	}
	return "file://" + repo, fixtureHEAD(t, repo)
}

// fixtureRepo writes the given file tree into dir and commits it with a
// fixture identity.
func fixtureRepo(t *testing.T, dir string, files map[string]string) error {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			return err
		}
	}
	git := func(args ...string) {
		t.Helper()
		all := append([]string{"-C", dir, "-c", "user.name=Fixtures", "-c", "user.email=fixtures@example.invalid"}, args...)
		out, err := stdexec.Command("git", all...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "fixture specs")
	return nil
}

// fixtureHEAD prints the committed HEAD sha.
func fixtureHEAD(t *testing.T, repo string) string {
	t.Helper()
	out, err := stdexec.Command("git", "-C", repo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// specControllerFix is the fixture controller spec — matching the
// bootstrap deployment's image (the adoption's noop plan).
const specControllerFix = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: top-controller
  namespace: core
spec:
  source:
    image: ghcr.io/toledompm/top/topctl
    tag: v0.1.0
  ports:
    - { name: http, containerPort: 8443 }
  routing:
    mode: internal
  env:
    TZ: UTC
  restartPolicy: always
`

func TestInstallCLIFullRunEnvelope(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dataDir := filepath.Join(base, "top")
	cfg := writeTestConfig(t, dataDir)
	repoURL, wantRev := seedRepoFixture(t)
	// T-406 split-runner: gitsource runs for real against the file://
	// fixture; the store opens for the phase-5 handoff; dokku answers
	// come from the fake.
	f := exectest.NewFakeRunner()
	seedFake(t, f)
	runner := splitGitRunner{f: f}
	key, kh := writeKeyFixtures(t, base)

	args := []string{
		"-o", "json", "--config", cfg, "--confirm",
		"install", repoURL,
		"--deploy-key", key, "--known-hosts", kh,
	}
	stdout, errOut, code := runCLIOpts(t, Options{Runner: runner}, args...)
	if code != ExitOK {
		t.Fatalf("full install exit = %d\nstderr: %s\nstdout: %s", code, errOut, stdout)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(strings.TrimSuffix(stdout, "\n")), &env); err != nil {
		t.Fatalf("stdout not the envelope: %v\n%s", err, stdout)
	}
	if env.Command != "install" || env.SchemaVersion != JSONSchemaVersion {
		t.Fatalf("envelope = %+v", env)
	}
	for _, k := range []string{"phasesRun", "created", "leftAlone", "checked", "nextCommands"} {
		if _, ok := env.Data[k]; !ok {
			t.Errorf("envelope data lacks %q: %+v", k, env.Data)
		}
	}
	// The host fakes were used: dataDir was created (0700 dirs listed in
	// created list).
	if !slices.ContainsFunc(env.Data["created"].([]any), func(v any) bool {
		s, _ := v.(string)
		return strings.HasPrefix(s, "app core-top-controller")
	}) {
		t.Errorf("created list must record the controller app: %+v", env.Data["created"])
	}
	_ = wantRev
}

// TestInstallCLIGate: non-TTY without --confirm → 2 (the gate), even
// with a good repo arg; a bad repo arg after consent → 2 (usage).
func TestInstallCLIGateAndValidation(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	cfg := writeTestConfig(t, filepath.Join(t.TempDir(), "top"))
	_, _, code := runCLIOpts(t, Options{Runner: f},
		"install", "https://git.example.com/example/platform.git", "--config", cfg)
	if code != ExitUsage {
		t.Fatalf("install gate exit = %d, want 2", code)
	}
	_, _, code = runCLIOpts(t, Options{Runner: f},
		"install", "github.com/example/top", "--config", cfg, "--confirm")
	if code != ExitUsage {
		t.Fatalf("bad repo arg exit = %d, want 2", code)
	}
}

// TestUninstallCLINothingInstalled: the default dataDir has no state on
// a test host (temp config) → exit 1 with the instructed finding.
func TestUninstallCLINothingInstalled(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	cfg := writeTestConfig(t, filepath.Join(t.TempDir(), "top"))
	stdout, errOut, code := runCLIOpts(t, Options{Runner: f},
		"-o", "json", "--config", cfg, "--confirm", "uninstall")
	if code != ExitDrift {
		t.Fatalf("uninstall no-state exit = %d, want 1\nstderr: %s\nstdout: %s", code, errOut, stdout)
	}
	var env struct {
		SchemaVersion int    `json:"schemaVersion"`
		Command       string `json:"command"`
		Error         *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	// Failure envelopes are written to the ERR stream by design
	// (out/err separation); stdout stays empty.
	if err := json.Unmarshal([]byte(strings.TrimSuffix(errOut, "\n")), &env); err != nil {
		t.Fatalf("stderr is the error envelope; parse: %v\nstderr: %s\nstdout: %s", err, errOut, stdout)
	}
	if env.Error.Code != "preflight-failed" {
		t.Errorf("error code = %q, want preflight-failed", env.Error.Code)
	}
}

// TestUninstallCLIDryRunPlan: with a REAL migrated store seeded at the
// temp dataDir (and the ledger showing one installed app), the dry-run
// prints the removal plan and mutates nothing.
func TestUninstallCLIDryRunPlan(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dataDir := filepath.Join(base, "top")
	cfg := writeTestConfig(t, dataDir)
	seedInstalledStore(t, dataDir)

	f := exectest.NewFakeRunner()
	f.SetPrefix([]string{"dokku", "version"}, exectest.Response{Stdout: []byte("dokku version 0.38.32")})
	f.SetPrefix([]string{"dokku", "apps:list"}, exectest.Response{Stdout: []byte(`["core-top-controller"]`)})
	f.SetPrefix([]string{"docker", "version"}, exectest.Response{Stdout: []byte("27.0.3")})
	f.SetPrefix([]string{"sudo"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"docker", "ps"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"docker", "network"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"docker", "volume"}, exectest.Response{Stdout: []byte("")})
	f.SetPrefix([]string{"ss"}, exectest.Response{Stdout: []byte("")})

	stdout, _, code := runCLIOpts(t, Options{Runner: f},
		"-o", "json", "--config", cfg, "--confirm", "uninstall", "--dry-run")
	if code != ExitOK {
		t.Fatalf("dry-run exit = %d", code)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(strings.TrimSuffix(stdout, "\n")), &env); err != nil {
		t.Fatalf("stdout not the envelope: %v\n%s", err, stdout)
	}
	var removed []any
	if list, ok := env.Data["removed"].([]any); ok {
		removed = list
	}
	if !slices.ContainsFunc(removed, func(v any) bool {
		s, _ := v.(string)
		return strings.Contains(s, "core-top-controller")
	}) {
		t.Errorf("dry-run plan must list the controller app: %+v", env.Data)
	}
	for _, c := range f.Recorded() {
		if slices.Contains(c.Args, "apps:destroy") {
			t.Fatalf("dry-run destroyed: %s", c.String())
		}
	}
}
