// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Fixture discipline (testing-strategy tier 1, mirrored from
// internal/doctor and internal/store/sqlite tests): everything hermetic.
// Runner = exectest.FakeRunner (never a real dokku, never a real host
// mutation); store = per-test temp sqlite via the real driver (the same
// pattern as the store's own openTestAt); clock and disk probe are
// injected constants; DataDir is always a t.TempDir().

// controllerAppFixture is the app name the checks must target.
const controllerAppFixture = "core-top-controller"

// okResp resp.
func okResp(s string) exectest.Response { return exectest.Response{Stdout: []byte(s)} }

// notFound is the dokku-absent response (exec's classified missing
// binary error, the same classification FastExec produces).
var notFound = exectest.Response{Err: exec.ErrNotFound}

// reportJSON is a ps:report --format json body with the read-only
// deployed/running flags (the prefix-less 0.38.x keys; report.go's
// verified parsing notes).
func reportJSON(deployed, running bool) string {
	var b strings.Builder
	b.WriteString(`{"app":"` + controllerAppFixture + `","deployed":`)
	b.WriteString(boolLit(deployed))
	b.WriteString(`,"running":`)
	b.WriteString(boolLit(running))
	b.WriteString(`,"processes":"1"}`)
	return b.String()
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// fixedNow freezes the clock for staleness math (deterministic goldens).
func fixedNow() func() time.Time {
	fixed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return fixed }
}

// testOptions builds the standard hermetic Options.
func testOptions(t *testing.T, runner *exectest.FakeRunner, dataDir string, diskFree uint64) Options {
	t.Helper()
	return Options{
		Runner:         runner,
		DataDir:        dataDir,
		GitURL:         "https://github.com/example/top",
		GitBranch:      "main",
		ConfigFile:     dataDir + "/top-config.yaml",
		BootstrapImage: "ghcr.io/example/top/topctl:bootstrap-test",
		DiskProbe:      func(string) (uint64, error) { return diskFree, nil },
		Now:            fixedNow(),
		PollInterval:   time.Millisecond,
		HealthTimeout:  50 * time.Millisecond,
	}
}

// diagnoseByIndex runs diagnose and returns its findings indexed by ID
// (the agent-facing key must be unique — asserted here).
func diagnoseByIndex(t *testing.T, opts Options) map[string]Finding {
	t.Helper()
	rep := Diagnose(context.Background(), opts)
	byID := map[string]Finding{}
	for _, f := range rep.Findings {
		if _, dup := byID[f.ID]; dup {
			t.Fatalf("duplicate finding id %q — IDs must be unique per run", f.ID)
		}
		byID[f.ID] = f
	}
	return byID
}

// wantID asserts one finding's existence + severity + non-empty hint for
// every non-ok severity (the ADR-0009 "scan printed fixes" discipline).
func wantID(t *testing.T, byID map[string]Finding, id string, sev Severity) Finding {
	t.Helper()
	f, ok := byID[id]
	if !ok {
		t.Fatalf("finding %q missing (have %+v)", id, byID)
	}
	if f.Sev() != sev {
		t.Errorf("finding %q severity %q, want %q (msg: %s)", id, f.Sev(), sev, f.Message)
	}
	if sev != sevOK && f.Hint == "" {
		t.Errorf("finding %q is %q with an empty hint — every non-ok finding must carry a suggested action", id, sev)
	}
	return f
}

// TestDiagnoseBareTempDirIndependence — THE independence invariant
// test: diagnose runs against a bare tempdir with a runner whose every
// dokku invocation fails as "binary not found" (no dokku installed),
// and produces informative doctor-shaped findings while touching
// nothing — no file appears in the dataDir, and every recorded
// subprocess attempt is a dokku read-only verb.
func TestDiagnoseBareTempDirIndependence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := exectest.NewFakeRunner()
	runner.SetDefault(notFound)
	opts := testOptions(t, runner, dir, 500<<30)

	byID := diagnoseByIndex(t, opts)
	wantID(t, byID, "dokku/present", sevFail)
	wantID(t, byID, "controller/state", sevInfo)
	wantID(t, byID, "store/missing", sevInfo)
	wantID(t, byID, "config/missing", sevInfo)
	wantID(t, byID, "perms/top", sevWarn) // tempdirs are 0755; the fresh-install contract is 0700
	wantID(t, byID, "disk/headroom", sevOK)

	// The operator's tree: untouched.
	for _, name := range dirNames(t, dir) {
		t.Errorf("diagnose wrote in the fresh dataDir: %s", name)
	}

	// Every attempt was dokku, and read-only.
	for _, c := range runner.Recorded() {
		if c.Name != "dokku" {
			t.Errorf("non-dokku subprocess from diagnose: %s", c.String())
			continue
		}
		if len(c.Args) == 0 || (c.Args[0] != "version" && c.Args[0] != "apps:list") {
			t.Errorf("diagnose invoked a dokku command that is not on the read-only allow-list: %s", c.String())
		}
	}
}

// TestDiagnoseHealthyAllClear — dokku healthy, controller deployed and
// running, a real migrated store on disk → zero fail findings and the
// every-check positive set.
func TestDiagnoseHealthyAllClear(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", okResp("dokku version 0.38.31 (git commit abc)"), "version")
	runner.SetOutput("dokku", okResp("=====> core-top-controller\n=====> core-something"), "apps:list")
	runner.SetOutput("dokku", okResp(reportJSON(true, true)), "ps:report", controllerAppFixture, "--format", "json")
	runner.SetOutput("dokku", okResp("no failed deploys\n"), "logs:failed", controllerAppFixture)
	runner.SetDefault(okResp(""))
	opts := testOptions(t, runner, dir, 500<<30)

	// The migrated temp db (real driver, store's own open path).
	st, err := sqlite.Open(context.Background(), filepath.Join(dir, "top.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	byID := diagnoseByIndex(t, opts)
	wantID(t, byID, "dokku/health", sevOK)
	wantID(t, byID, "controller/state", sevOK)
	wantID(t, byID, "store/integrity", sevOK)
	wantID(t, byID, "config/missing", sevInfo)
	for id, f := range byID {
		if f.Sev() == sevFail {
			t.Errorf("healthy fixture produced a fail: %s: %s", id, f.Message)
		}
	}
}

// TestDiagnoseControllerDeadWithLogsTail — the repair-suggestion
// contract: a dead controller gets a fail whose hint names
// `topctl recover restart` and the newest log line surfaces as the
// evidence finding (the contract example's "last log lines suggest:").
func TestDiagnoseControllerDeadWithLogsTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", okResp("dokku version 0.38.31 (git commit abc)"), "version")
	runner.SetOutput("dokku", okResp("=====> core-top-controller"), "apps:list")
	runner.SetOutput("dokku", okResp(reportJSON(true, false)), "ps:report", controllerAppFixture, "--format", "json")
	runner.SetOutput("dokku",
		okResp("top-controller[1]: cannot open /var/lib/top/top.db: disk I/O error\n"),
		"logs", controllerAppFixture, "--num", "200")
	runner.SetDefault(okResp(""))
	opts := testOptions(t, runner, dir, 500<<30)

	byID := diagnoseByIndex(t, opts)
	st := wantID(t, byID, "controller/state", sevFail)
	if !strings.Contains(st.Hint, "topctl recover restart") {
		t.Errorf("dead-controller hint lacks the restart runbook line: %q", st.Hint)
	}
	tail := wantID(t, byID, "controller/log-tail", sevInfo)
	if !strings.Contains(tail.Message, "disk I/O error") {
		t.Errorf("log tail not surfaced as evidence: %+v", tail)
	}
}

// TestDiagnoseControllerMissing — app never deployed → fail with the
// redeploy runbook line (and the store freshness stays info).
func TestDiagnoseControllerMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", okResp("dokku version 0.38.31 (git commit abc)"), "version")
	runner.SetOutput("dokku", okResp("=====> some-other-app"), "apps:list")
	runner.SetDefault(okResp(""))
	opts := testOptions(t, runner, dir, 500<<30)

	byID := diagnoseByIndex(t, opts)
	f := wantID(t, byID, "controller/missing", sevFail)
	if !strings.Contains(f.Hint, "topctl recover redeploy") {
		t.Errorf("missing-controller hint lacks the redeploy runbook line: %q", f.Hint)
	}
	wantID(t, byID, "store/missing", sevInfo)
}

// TestDiagnoseStoreStaleAndSchemaNewer — the two store diagnosis paths
// the task calls out: staleness warn on an old newest-op, and the typed
// ErrSchemaNewer open failure as its own fail-severity finding.
func TestDiagnoseStoreStaleAndSchemaNewer(t *testing.T) {
	t.Parallel()

	t.Run("fresh-store", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runner := exectest.NewFakeRunner()
		runner.SetDefault(exectest.Response{}) // empty stdout, exit 0
		opts := testOptions(t, runner, dir, 500<<30)

		writeMigratedStore(t, filepath.Join(dir, "top.db"))
		byID := diagnoseByIndex(t, opts)
		wantID(t, byID, "store/integrity", sevOK)
	})

	t.Run("stale-newest-op", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runner := exectest.NewFakeRunner()
		runner.SetDefault(exectest.Response{})
		opts := testOptions(t, runner, dir, 500<<30)

		db := filepath.Join(dir, "top.db")
		writeMigratedStore(t, db)
		writeOldOp(t, db, fixedNow()().Add(-41*time.Hour))
		byID := diagnoseByIndex(t, opts)
		f := wantID(t, byID, "store/integrity", sevWarn)
		if !strings.Contains(f.Message, "41h0m0s old") || !strings.Contains(f.Message, "BUT stale") {
			t.Errorf("stale wording drifted: %+v", f)
		}
	})

	t.Run("schema-newer", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		runner := exectest.NewFakeRunner()
		runner.SetDefault(exectest.Response{})
		opts := testOptions(t, runner, dir, 500<<30)

		db := filepath.Join(dir, "top.db")
		writeMigratedStore(t, db)
		bumpSchemaVersion(t, db, 99)
		byID := diagnoseByIndex(t, opts)
		f := wantID(t, byID, "store/schema-newer", sevFail)
		if !strings.Contains(f.Message, "schema v99") {
			t.Errorf("typed schema-newer info missing: %+v", f)
		}
	})
}

// TestDiagnoseConfigAndPerms — config valid/invalid/missing and the
// read-only permission checks fan-out (loose dist/dir warn, fresh info).
func TestDiagnoseConfigAndPerms(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	runner := exectest.NewFakeRunner()
	runner.SetDefault(exectest.Response{})
	opts := testOptions(t, runner, dir, 500<<30)

	// Missing config → fresh-install info.
	byID := diagnoseByIndex(t, opts)
	wantID(t, byID, "config/missing", sevInfo)

	// An invalid config → fail (strict parse: typos fail loudly).
	bad := dir + "/top-config.yaml"
	writeFile(t, bad, "apiVersion: wrong/none\n")
	byID = diagnoseByIndex(t, opts)
	f := wantID(t, byID, "config/invalid", sevFail)
	if f.Message == "" {
		t.Errorf("config fail should carry the parse diagnosis")
	}

	// A valid config → ok.
	writeFile(t, bad, "apiVersion: top/v1alpha1\nlogLevel: debug\n")
	byID = diagnoseByIndex(t, opts)
	wantID(t, byID, "config/valid", sevOK)

	// Permission fan-out on the fresh host: the loose dirs case.
	secrets := filepath.Join(dir, "secrets")
	mkdirAll(t, secrets, 0o755)
	writeFileWithMode(t, secrets+"/token", 0o644, "supersecret-not-read-by-diagnose")
	loose := filepath.Join(dir, "top-config.yaml")
	t.Logf("%s remains the valid config", loose)
	byID = diagnoseByIndex(t, opts)
	wantID(t, byID, "perms/secrets", sevWarn)
	sf := wantID(t, byID, "perms/secrets-file", sevWarn)
	if !strings.Contains(sf.Hint, "chmod 600") {
		t.Errorf("loose secret file hint is not a chmod suggestion: %q", sf.Hint)
	}
	if sf.Message != "" && containsAny(sf.Message, "supersecret") {
		t.Errorf("secret LEAKED into a finding message: %+v", sf)
	}
}

// ---- tiny fs helpers -------------------------------------------------------
