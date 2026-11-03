// SPDX-License-Identifier: MIT

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// T-406: the `topctl deploy core/top-controller` self-deploy matrix.
// The reconciler's own refusal on unapproved/unadopted runs is pinned in
// internal/reconcile (TestAdoptSelfImageDriftHumanGated,
// TestSelfDeploy* in adopt_selfgate_test.go); these rows pin the CLI
// half: the confirm gate, the explicit-human apply, and the old-release
// preservation + rollback echo. Fixture values are fictional; dokku
// answers come from the FakeRunner, git from a file:// fixture repo.

// specControllerNewTag is the fixture controller spec with the NEW image
// tag — the revision change the human gate decides about.
const specControllerNewTag = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: top-controller
  namespace: core
spec:
  source:
    image: ghcr.io/example/top/topctl
    tag: v2
  ports:
    - { name: http, containerPort: 8443 }
  routing:
    mode: internal
  env:
    TZ: UTC
  restartPolicy: always
`

// seedControllerRepo commits a controller spec into a local fixture
// platform repo and returns its file:// URL (both the deploy command's
// repo scope via the config's git.url and the positional-backed
// composition share it).
func seedControllerRepo(t *testing.T, spec string) string {
	t.Helper()
	repo := t.TempDir() + "/platform-repo"
	if err := fixtureRepo(t, repo, map[string]string{"core/top-controller.yaml": spec}); err != nil {
		t.Fatal(err)
	}
	return "file://" + repo
}

// writeTestConfigGit is writeTestConfig with an explicit git url (the
// fixture repo): the deploy command resolves desired state through
// orGitSource, so config and fixture must agree on the URL.
func writeTestConfigGit(t *testing.T, dataDir, url string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "top-config.yaml")
	yaml := "apiVersion: top/v1alpha1\ndataDir: " + dataDir + "\n" +
		"git:\n  url: " + url + "\n  branch: main\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// seedSelfDeployEnv builds one fully prepared self-deploy fixture:
// adopted (managed) controller row + the dokku fake teaching ONE app
// running the OLD bootstrap image, with git:report scripted so the FIRST
// Inspect (observe → plan drift) reports the old image and the tail —
// the readiness polls after the DeployImage verb — reports the new tag.
func seedSelfDeployEnv(t *testing.T, spec string) (cfg, repoURL string, f *exectest.FakeRunner) {
	t.Helper()
	repoURL = seedControllerRepo(t, spec)
	base := t.TempDir()
	dataDir := filepath.Join(base, "top")
	cfg = writeTestConfigGit(t, dataDir, repoURL)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), filepath.Join(dataDir, "top.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	err = st.InsertLedgerEntry(t.Context(), &store.Resource{
		Kind:        "app",
		Identifier:  "core-top-controller",
		Labels:      map[string]string{"top.dev/managed": "true", "top.dev/namespace": "core", "top.dev/service": "top-controller"},
		CreatedByOp: "fixture-op-install",
		State:       store.ResourceManaged,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f = exectest.NewFakeRunner()
	seedFake(t, f)
	f.SetScriptedPrefix([]string{"dokku", "git:report"}, []exectest.Response{
		{Stdout: []byte(`{"app":"core-top-controller","source-image":"ghcr.io/example/top/topctl:bootstrap-pinned"}`)},
	}, exectest.Response{Stdout: []byte(`{"app":"core-top-controller","source-image":"ghcr.io/example/top/topctl:v2"}`)})
	f.SetScriptedPrefix([]string{"dokku", "apps:report", "core-top-controller"}, []exectest.Response{
		{Stdout: []byte(`{"app":"core-top-controller","deploy-source":"docker-image","deploy-source-metadata":"ghcr.io/example/top/topctl:bootstrap-pinned","locked":"false"}`)},
	}, exectest.Response{Stdout: []byte(`{"app":"core-top-controller","deploy-source":"docker-image","deploy-source-metadata":"ghcr.io/example/top/topctl:v2","locked":"false"}`)})
	return cfg, repoURL, f
}

func wasDeployedWith(f *exectest.FakeRunner, image string) bool {
	for _, c := range f.Recorded() {
		if c.Name == "dokku" && slices.Contains(c.Args, "git:from-image") && len(c.Args) > 0 && c.Args[len(c.Args)-1] == image {
			return true
		}
	}
	return false
}

// TestDeploySelfHumanGateMatrix — the gate rows:
//
//   - no --confirm, non-TTY → exit 2 (usage), zero backend verbs
//   - --confirm → the explicit-human run applies the new image
//     (git:from-image with the v2 reference), exit 0, preservation echo
//   - interactive TTY "y" answer is the same human approval path
func TestDeploySelfHumanGateMatrix(t *testing.T) {
	t.Run("non-tty-no-confirm-is-usage", func(t *testing.T) {
		cfg, _, f := seedSelfDeployEnv(t, specControllerNewTag)
		stdout, errOut, code := runCLIOpts(t, Options{Runner: splitGitRunner{f: f}},
			"deploy", "core/top-controller", "--config", cfg)
		if code != ExitUsage {
			t.Fatalf("exit = %d, want 2\nerr: %s\nout: %s", code, errOut, stdout)
		}
		if !strings.Contains(errOut, "--confirm") {
			t.Errorf("usage error must name --confirm: %s", errOut)
		}
		for _, c := range f.Recorded() {
			if slices.Contains(c.Args, "git:from-image") {
				t.Errorf("nothing may deploy without consent: %s", c.String())
			}
		}
	})

	t.Run("confirmed-human-apply", func(t *testing.T) {
		cfg, _, f := seedSelfDeployEnv(t, specControllerNewTag)
		stdout, errOut, code := runCLIOpts(t, Options{Runner: splitGitRunner{f: f}},
			"deploy", "core/top-controller", "--confirm", "--config", cfg)
		if code != ExitOK {
			t.Fatalf("exit = %d, want 0\nerr: %s\nout: %s", code, errOut, stdout)
		}
		if !wasDeployedWith(f, "ghcr.io/example/top/topctl:v2") {
			t.Errorf("the human self-deploy must run git:from-image with the new image")
		}
		if !strings.Contains(stdout, "previous release keeps serving") ||
			!strings.Contains(stdout, "topctl recover redeploy") {
			t.Errorf("the preservation + rollback echo must print:\n%s", stdout)
		}
	})

	t.Run("interactive-tty-yes-applies", func(t *testing.T) {
		cfg, _, f := seedSelfDeployEnv(t, specControllerNewTag)
		tty := true
		opts := Options{Runner: splitGitRunner{f: f}, StdinIsTTY: &tty, Stdin: strings.NewReader("y\n")}
		_, _, _ = runCLIOpts(t, opts, "deploy", "core/top-controller", "--config", cfg)
		if !wasDeployedWith(f, "ghcr.io/example/top/topctl:v2") {
			t.Errorf("a confirming y-answer must carry the same human approval (the deploy must run)")
		}
	})
}

// TestDeploySelfFailureKeepsOldRelease: the deploy VERB itself fails
// (git:from-image exit 1) → exit 3, the preservation echo still printed
// (it precedes the run), and nothing destructive touches the app — the
// old release keeps serving (no apps:destroy anywhere in the log).
func TestDeploySelfFailureKeepsOldRelease(t *testing.T) {
	t.Parallel()
	cfg, _, f := seedSelfDeployEnv(t, specControllerNewTag)
	f.SetPrefix([]string{"dokku", "git:from-image"}, exectest.Response{ExitCode: 1})

	stdout, _, code := runCLIOpts(t, Options{Runner: splitGitRunner{f: f}},
		"deploy", "core/top-controller", "--confirm", "--config", cfg)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 3", code)
	}
	if !strings.Contains(stdout, "previous release keeps serving") {
		t.Errorf("the preservation echo must print even when the run fails:\n%s", stdout)
	}
	for _, c := range f.Recorded() {
		if slices.Contains(c.Args, "apps:destroy") {
			t.Errorf("a failed self-deploy must not remove anything: %s", c.String())
		}
	}
}
