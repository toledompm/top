// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/store"
)

// Tests for the mutating actions. DANGER LINE (task guard): the fake
// runner is the ONLY seam — no test here may let a real `dokku`
// mutation run. Every test injects Options.Runner = exectest.FakeRunner
// via testOptions; the CLI-level wire tests (internal/cli/recover_test.go)
// keep the same discipline through cli.Options.Runner.

func mustStore(t *testing.T, dir string) store.Store {
	t.Helper()
	return writeMigratedStore(t, filepath.Join(dir, "top.db"))
}

func setHealthyReport(t *testing.T, runner *exectest.FakeRunner) {
	t.Helper()
	runner.SetOutput("dokku", okResp(reportJSON(true, true)),
		"ps:report", controllerAppFixture, "--format", "json")
}

// recordLike joins the recorded call log for assertions.
func recordLike(runner *exectest.FakeRunner) string {
	return strings.Join(runner.RecordedCommands(), "\n")
}

func TestRestartSuccess(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", okResp("!!! restarted"), "ps:restart", controllerAppFixture)
	setHealthyReport(t, runner)
	opts := testOptions(t, runner, t.TempDir(), 500<<30)

	res, err := Restart(context.Background(), opts, false)
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if res.App != controllerAppFixture || len(res.Commands) != 1 {
		t.Fatalf("result: %+v", res)
	}
	want := "dokku ps:restart core-top-controller\n" +
		"dokku ps:report core-top-controller --format json"
	if got := recordLike(runner); got != want {
		t.Errorf("command log:\n%s\nwant:\n%s", got, want)
	}
}

func TestRestartHealthTimeout(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", okResp("!!! restarted"), "ps:restart", controllerAppFixture)
	// The app restarts but never comes up.
	runner.SetOutput("dokku", okResp(reportJSON(true, false)),
		"ps:report", controllerAppFixture, "--format", "json")
	opts := testOptions(t, runner, t.TempDir(), 500<<30)

	_, err := Restart(context.Background(), opts, false)
	if err == nil {
		t.Fatal("want a bounded-timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "not healthy within") {
		t.Errorf("error lacks the bounded-wait wording: %v", err)
	}
}

func TestRestartDokkuFailureCarriesRetry(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", exectest.Response{
		Stderr:   []byte("port in use"),
		ExitCode: 1,
	}, "ps:restart", controllerAppFixture)
	opts := testOptions(t, runner, t.TempDir(), 500<<30)

	res, err := Restart(context.Background(), opts, false)
	if err == nil {
		t.Fatal("want the restart failure, got nil")
	}
	if res == nil || len(res.Commands) != 1 {
		t.Fatalf("even failures must carry the runbook commands: %+v", res)
	}
	if !strings.Contains(err.Error(), "dokku ps:restart") {
		t.Errorf("failure lacks the exact retry command: %v", err)
	}
}

func TestRestartDryRunMutatesNothing(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	opts := testOptions(t, runner, t.TempDir(), 500<<30)

	res, err := Restart(context.Background(), opts, true)
	if err != nil {
		t.Fatalf("dry-run Restart: %v", err)
	}
	if calls := runner.Recorded(); len(calls) != 0 {
		t.Errorf("dry-run executed %d commands: %v", len(calls), calls)
	}
	want := "dokku ps:restart core-top-controller"
	if len(res.Commands) != 1 || res.Commands[0] != want {
		t.Errorf("dry-run result commands = %v, want [%q]", res.Commands, want)
	}
}

// TestRedeployFromStoreCheckpoint — the default resolution path: record
// a repo-wide last-known-good in a real temp store, then redeploy; the
// store's revision must drive git:sync + ps:rebuild.
func TestRedeployFromStoreCheckpoint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := mustStore(t, dir)
	ctx := context.Background()
	const sha = "abc123def4560"
	if err := st.SetCheckpoint(ctx, &store.Checkpoint{LastKnownGood: sha}); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	runner := exectest.NewFakeRunner()
	setHealthyReport(t, runner)
	opts := testOptions(t, runner, dir, 500<<30)

	res, err := Redeploy(ctx, opts, "", false)
	if err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if res.Revision != sha {
		t.Errorf("resolved revision = %q, want %q", res.Revision, sha)
	}
	if !strings.Contains(res.Source, "store checkpoint") {
		t.Errorf("decision provenance not surfaced: %q", res.Source)
	}
	got := recordLike(runner)
	if !strings.Contains(got, "dokku git:sync core-top-controller https://github.com/example/top "+sha+"\n") ||
		!strings.Contains(got, "dokku ps:rebuild core-top-controller") {
		t.Errorf("command log lacks the sync/rebuild pair:\n%s", got)
	}
}

// TestRedeployPerServiceImageLKG — a per-service IMAGE last-known-good
// deploys via git:from-image of that image (image deploys carry image
// refs, not shas — domain.RevisionRef semantics; ADR-0005).
func TestRedeployPerServiceImageLKG(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st := mustStore(t, dir)
	ctx := context.Background()
	const img = "ghcr.io/example/top/topctl@sha256:0123"
	if err := st.SetCheckpoint(ctx, &store.Checkpoint{
		LastKnownGoodPerService: map[domain.ServiceRef]domain.RevisionRef{
			controllerRef: {Kind: domain.RevisionImage, Value: img},
		},
	}); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	runner := exectest.NewFakeRunner()
	setHealthyReport(t, runner)
	opts := testOptions(t, runner, dir, 500<<30)

	res, err := Redeploy(ctx, opts, "", false)
	if err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if res.Image != img || res.Revision != "" {
		t.Errorf("resolved deploy = rev %q / image %q, want image %q", res.Revision, res.Image, img)
	}
	if got := recordLike(runner); !strings.Contains(got, "dokku git:from-image core-top-controller "+img) {
		t.Errorf("command log lacks the from-image deploy:\n%s", got)
	}
}

// TestRedeployFallsBackToBootstrapWhenStoreEmpty — no checkpoint yet
// (fresh install / never-green reconcile) → the pinned bootstrap image.
func TestRedeployFallsBackToBootstrapWhenStoreEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = mustStore(t, dir) // exists but has no checkpoint yet

	runner := exectest.NewFakeRunner()
	setHealthyReport(t, runner)
	opts := testOptions(t, runner, dir, 500<<30)

	res, err := Redeploy(context.Background(), opts, "", false)
	if err != nil {
		t.Fatalf("Redeploy: %v", err)
	}
	if res.Image != "ghcr.io/example/top/topctl:bootstrap-test" {
		t.Errorf("bootstrap fallback image = %q", res.Image)
	}
	if got := recordLike(runner); !strings.Contains(got, "dokku git:from-image core-top-controller ghcr.io/example/top/topctl:bootstrap-test") {
		t.Errorf("command log lacks the from-image deploy:\n%s", got)
	}
}

// TestRedeploySurvivesBrokenStore — the store file is corrupt/unopenable
// → the redeploy still runs, from the pinned image, and the note says why
// (ADR-0005: recovery depends on the SQLite FILE, not on it being good).
func TestRedeploySurvivesBrokenStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "top.db"), "this is not a database")
	runner := exectest.NewFakeRunner()
	setHealthyReport(t, runner)
	opts := testOptions(t, runner, dir, 500<<30)

	res, err := Redeploy(context.Background(), opts, "", false)
	if err != nil {
		t.Fatalf("broken store must not block a pinned-image redeploy: %v", err)
	}
	if res.Image != "ghcr.io/example/top/topctl:bootstrap-test" {
		t.Errorf("fallback image = %q", res.Image)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "could not be opened") {
		t.Errorf("the store failure must be surfaced as a note: %+v", res.Notes)
	}
}

func TestRedeployRevisionFlagNeedsGitURL(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	opts := testOptions(t, runner, t.TempDir(), 500<<30)
	opts.GitURL = "" // operator never registered the repo

	_, err := Redeploy(context.Background(), opts, "abc123def4560", false)
	if err == nil || !strings.Contains(err.Error(), "no platform repo URL") {
		t.Fatalf("want the git.url refusal, got %v", err)
	}
	for _, c := range runner.Recorded() {
		t.Errorf("nothing should have run: %s", c.String())
	}
}

func TestRedeployDryRun(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	opts := testOptions(t, runner, t.TempDir(), 500<<30)

	res, err := Redeploy(context.Background(), opts, "abc123def4560", true)
	if err != nil {
		t.Fatalf("dry-run Redeploy: %v", err)
	}
	if calls := runner.Recorded(); len(calls) != 0 {
		t.Errorf("dry-run executed %d commands: %v", len(calls), calls)
	}
	joined := strings.Join(res.Commands, "\n")
	if !strings.Contains(joined, "dokku git:sync core-top-controller https://github.com/example/top abc123def4560") ||
		!strings.Contains(joined, "dokku ps:rebuild core-top-controller") {
		t.Errorf("dry-run plan lacks the command pair:\n%s", joined)
	}
}

// The health wait honors context cancellation (bounded, not forever).
func TestWaitHealthyHonorsContext(t *testing.T) {
	t.Parallel()
	runner := exectest.NewFakeRunner()
	runner.SetDefault(exectest.Response{Stdout: []byte(reportJSON(true, false))})
	opts := testOptions(t, runner, t.TempDir(), 500<<30)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := waitHealthy(ctx, opts, controllerAppFixture); err == nil {
		t.Fatal("want a context-deadline error")
	} else if !strings.Contains(err.Error(), "manual check") {
		t.Errorf("cancellation error lacks the manual check line: %v", err)
	}
}
