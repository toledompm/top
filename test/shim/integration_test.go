//go:build toptests

// Tier-2 shim integration test (ADR-0012): production exec.FastExec resolves
// the fake `dokku` script (test/shim/bin/dokku) via PATH, argv is recorded to
// a newline-JSON log, and canned responses come from the fixture registry.
// The subtests cover the representative calls listed in
// docs/contracts/platform-backend.md.
//
// Deliberately NOT part of `go test ./...`/CI: run via `make test-shim`.
// Usage snippet, failure-injection grammar and registry precedence live in
// test/shim/README.md.
package shim_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/test/fakeexec"
)

const (
	shimName        = "dokku"
	secretTestValue = "s3cr3t-top-value-7731" // fixture value; must come back as "***"
)

// shimEnv builds the loop environment. FastExec's env hygiene strips the
// parent environment, so every DOKKU_SHIM_* variable must be explicitly
// forwarded via ExtraEnv; PATH arrives through DefaultEnvPassthrough.
// The returned stackDir is the registry directory for this test — install
// responses into it with installFixtures/installFamily.
func shimEnv(t *testing.T, extras ...string) (fe *exec.FastExec, log, stackDir string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "shim.log")
	stackDir = filepath.Join(dir, "stack")
	fe = &exec.FastExec{
		ExtraEnv: append([]string{
			"DOKKU_SHIM_LOG=" + log,
			"DOKKU_SHIM_FIXTURES=" + stackDir,
			"DOKKU_SHIM_STATE=" + filepath.Join(dir, "state"),
			"DOKKU_SHIM_FAIL_ONCE=",
		}, extras...),
		WaitDelay: 2 * time.Second,
	}
	return fe, log, stackDir
}

func installFixtures(t *testing.T, stackDir string, fss ...fakeexec.Fixture) {
	t.Helper()
	if err := fakeexec.WriteShimFixtures(stackDir, fss); err != nil {
		t.Fatal(err)
	}
}

func installFamily(t *testing.T, stackDir, family string, f fakeexec.Fixture) {
	t.Helper()
	if err := fakeexec.WriteShimFamily(stackDir, family, f); err != nil {
		t.Fatal(err)
	}
}

func installShim(t *testing.T) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(root, "test", "shim", "bin")
	// Fresh clones may have lost the exec bit; restore defensively.
	if err := os.Chmod(filepath.Join(shimDir, shimName), 0o755); err != nil {
		t.Fatalf("make shim executable: %v", err)
	}
	// FastExec resolves argv[0] against the test process' PATH, and PATH is
	// passthrough env — prepending routes `dokku` to bin/dokku.
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
}

func TestShimFullLoop(t *testing.T) {
	installShim(t)
	fxs, err := fakeexec.LoadFixtures(filepath.Join("..", "fixtures", "dokku"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]fakeexec.Fixture{}
	for _, f := range fxs {
		byKey[f.Key()] = f
	}
	ctx := context.Background()
	psReportKey := "dokku ps:report top-test-app --format json"
	configSetKey := "dokku config:set --no-restart top-test-app EXAMPLE_TOKEN=example-token-value"

	t.Run("apps:list --format json", func(t *testing.T) { // HealthyBackend / Inspect listing
		fe, log, stack := shimEnv(t)
		installFixtures(t, stack, byKey["dokku apps:list --format json"])

		stdout, stderr, err := fe.Run(ctx, shimName, "apps:list", "--format", "json")
		if err != nil {
			t.Fatalf("run: %v (stderr=%q)", err, stderr)
		}
		if !strings.Contains(string(stdout), `"top-test-app"`) {
			t.Errorf("stdout missing fixture payload: %q", stdout)
		}
		cmds, err := fakeexec.ParseLogFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if len(cmds) != 1 || !fakeexec.EqualArgv(shimName, []string{"apps:list", "--format", "json"}, cmds[0]) {
			t.Errorf("recorded argv = %+v, want the golden apps:list call", cmds)
		}
	})

	t.Run("ps:report exit mapping and redaction", func(t *testing.T) { // Inspect
		fe, _, stack := shimEnv(t)
		installFixtures(t, stack, byKey[psReportKey])
		fe.RedactSeed = []string{secretTestValue}

		stdout, stderr, err := fe.Run(ctx, shimName, "ps:report", "top-test-app", "--format", "json")
		if err != nil {
			t.Fatalf("run: %v (stderr=%q)", err, stderr)
		}
		if strings.Contains(string(stdout), secretTestValue) || !strings.Contains(string(stdout), "***") {
			t.Errorf("secret not redacted in stdout: %s", stdout)
		}

		// Same call, non-zero exit: typed ExitError carrying the stderr.
		res := byKey[psReportKey]
		res.Exit = 1
		res.Stdout, res.Stderr = "", " !     no such app: top-test-app\n"
		fe, _, stack = shimEnv(t)
		installFixtures(t, stack, res)
		_, stderr, err = fe.Run(ctx, shimName, "ps:report", "top-test-app", "--format", "json")
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("want *exec.ExitError, got %#v", err)
		}
		if ee.ExitCode != 1 || !exec.IsExitCode(err, 1) {
			t.Errorf("exit code mapping broken: %+v", ee)
		}
		if !strings.Contains(err.Error(), "ps:report") {
			t.Errorf("error lost the command label: %v", err)
		}
	})

	t.Run("config:set --no-restart with failure injection", func(t *testing.T) { // Apply batching
		fe, log, stack := shimEnv(t, "DOKKU_SHIM_FAIL_ONCE=config:set|37|1")
		installFixtures(t, stack, byKey[configSetKey])

		_, stderr, err := fe.Run(ctx, shimName, "config:set", "--no-restart", "top-test-app", "EXAMPLE_TOKEN=example-token-value")
		if !exec.IsExitCode(err, 37) {
			t.Fatalf("first call err = %v (stderr %q), want injected exit 37", err, stderr)
		}
		if !strings.Contains(string(stderr), "injected failure") {
			t.Errorf("injection reason missing from stderr: %q", stderr)
		}

		// Same Runner, second call: counter moved past 1, fixture applies.
		if _, _, err := fe.Run(ctx, shimName, "config:set", "--no-restart", "top-test-app", "EXAMPLE_TOKEN=example-token-value"); err != nil {
			t.Fatalf("retry after injected failure: %v", err)
		}

		cmds, err := fakeexec.ParseLogFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if len(cmds) != 2 || !fakeexec.EqualArgv(shimName,
			[]string{"config:set", "--no-restart", "top-test-app", "EXAMPLE_TOKEN=example-token-value"}, cmds[1]) {
			t.Errorf("recorded argv = %+v, want two config:set calls", cmds)
		}
	})

	t.Run("git:from-image fallback precedence", func(t *testing.T) { // deploys
		// Nothing configured: default response — success, empty output.
		fe, log, _ := shimEnv(t, "DOKKU_SHIM_FIXTURES=/nonexistent-6529")
		stdout, _, err := fe.Run(ctx, shimName, "git:from-image", "top-test-app", "ghcr.io/example/top-test-app:1.2.3")
		if err != nil {
			t.Fatalf("default fallback not success: %v", err)
		}
		if len(stdout) != 0 {
			t.Errorf("default fallback emitted stdout: %q", stdout)
		}
		if cmds, err := fakeexec.ParseLogFile(log); err != nil || len(cmds) != 1 ||
			!fakeexec.EqualArgv(shimName,
				[]string{"git:from-image", "top-test-app", "ghcr.io/example/top-test-app:1.2.3"}, cmds[0]) {
			t.Errorf("git:from-image argv not recorded: %+v %v", cmds, err)
		}

		// Family fallback ("git" — first arg up to ':') engages when nothing
		// more specific exists.
		fe, _, stack := shimEnv(t)
		installFamily(t, stack, "git", fakeexec.Fixture{Stderr: "family fallback\n", Exit: 9})
		_, stderr, err := fe.Run(ctx, shimName, "git:from-image", "top-test-app", "ghcr.io/example/top-test-app:1.2.4")
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode != 9 {
			t.Fatalf("family fallback err = %v, want exit 9", err)
		}
		if !strings.Contains(string(stderr), "family fallback") {
			t.Errorf("fallback stderr not surfaced: %q", stderr)
		}

		// Exact-argv fixture beats the family fallback.
		fe, _, stack = shimEnv(t)
		exact := fakeexec.Fixture{
			Name: shimName,
			Args: []string{"git:from-image", "top-test-app", "ghcr.io/example/top-test-app:1.2.5"},
		}
		installFamily(t, stack, "git", fakeexec.Fixture{Stderr: "family fallback\n", Exit: 9})
		installFixtures(t, stack, exact)
		if _, _, err := fe.Run(ctx, shimName, exact.Args...); err != nil {
			t.Errorf("exact fixture lost to family fallback: %v", err)
		}
	})

	t.Run("malformed usage recorded and refused", func(t *testing.T) {
		fe, log, _ := shimEnv(t)
		if _, _, err := fe.Run(ctx, shimName, "apps:list\n", ""); !exec.IsExitCode(err, 2) {
			t.Fatalf("malformed call err = %v, want exit 2", err)
		}
		cmds, err := fakeexec.ParseLogFile(log)
		if err != nil {
			t.Fatalf("log line not parseable despite malformed argv: %v", err)
		}
		if len(cmds) != 1 || len(cmds[0].Args) != 2 || !strings.Contains(cmds[0].Args[0], "apps:") {
			t.Errorf("offending argv not recorded: %+v", cmds)
		}
	})
}
