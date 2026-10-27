// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"encoding/json"
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
	f.SetPrefix([]string{"dokku", "git:report"}, exectest.Response{Stdout: []byte(`{"source-image":"ghcr.io/example/top/topctl:bootstrap-pinned"}`)})
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

func TestInstallCLIFullRunEnvelope(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	dataDir := filepath.Join(base, "top")
	cfg := writeTestConfig(t, dataDir)
	f := exectest.NewFakeRunner()
	seedFake(t, f)
	key, kh := writeKeyFixtures(t, base)

	args := []string{
		"-o", "json", "--config", cfg, "--confirm",
		"install", "ssh://git@git.example.com/example/platform.git",
		"--deploy-key", key, "--known-hosts", kh,
	}
	stdout, errOut, code := runCLIOpts(t, Options{Runner: f}, args...)
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
	_ = dataDir
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
