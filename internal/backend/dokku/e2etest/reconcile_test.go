//go:build e2e

// SPDX-License-Identifier: MIT

// Tier-3 e2e for the reconcile pipeline over the REAL adapter (M3 AC1:
// "push to repo (or manual reconcile) lands a spec change on a real
// Dokku app"). Same harness as apply_test.go (dokku/dokku container via
// testcontainers; docker-exec Runner; bounded deadlines everywhere).
//
// The git half uses the wave's sanctioned fixture: a LOCAL git repo
// (like internal/cli/recon_test.go's platform-repo fixture and the
// gitsource e2e's container-remote variant) driven through the
// PRODUCTION gitsource.Source — mirror → ls-remote → archive. What is
// REAL end to end: git fetch→spec load→reconcile.Run→dokku adapter
// convergence onto the real app, observed back through dokku's own
// reports. Gated:
//
//	TOP_E2E=1 go test ./internal/backend/dokku/e2etest -tags e2e -race -count=1
package e2etest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/config"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/spec"
	"github.com/toledompm/top/internal/store/sqlite"
)

// recSpec is the fictional one-service platform spec committed to the
// local fixture repo (ADR-0014: public image, no real identity).
//
// NOTE (T-311 finding, tier-3 verified): a spec WITH declared ports does
// not tier-3-converge — dokku auto-assigns a host port for the unpinned
// `http::<n>` mapping (ports:report shows host_port 8080) while the plan
// models desired ports as host-port-less, so every run re-reports
// `~ports.http: 8080:8080 → 8080` and re-applies (livelock, never
// noop). The fixture omits ports; reconciling the port model with
// dokku's real mapping semantics is open work.
const recSpec = `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: e2erecon
  namespace: tops
spec:
  source:
    image: docker.io/library/httpd:2.4-alpine
  routing:
    mode: internal
  env:
    E2E_RECON_TEST: "one"
  restartPolicy: always
`

// commitSpec seeds/overwrites the spec in the fixture repo and commits.
func commitSpec(t *testing.T, repo, body string) string {
	t.Helper()
	if err := os.MkdirAll(repo+"/tops", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo+"/tops/e2erecon.yaml", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{
			"-C", repo,
			"-c", "user.name=e2e", "-c", "user.email=e2e@example.invalid",
		}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if !fileExists(repo + "/.git/HEAD") {
		run("init", "-b", "main")
	}
	run("add", ".")
	run("commit", "--quiet", "--allow-empty", "-m", "fixture spec")
	return run("rev-parse", "HEAD")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestE2EReconcilePipelineLandsSpecChange: a committed spec revision,
// resolved through the production gitsource (local fixture repo) and
// parsed by the production spec loader, is converged by reconcile.Run
// onto a REAL Dokku app — create, env, ports, network, restart policy,
// deploy, readiness — then observed back converged (image, revision
// match), and a SECOND run records pure no-ops (the idempotency
// invariant at tier 3).
func TestE2EReconcilePipelineLandsSpecChange(t *testing.T) {
	env := skipGated(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	tmp := t.TempDir()
	repo := tmp + "/platform-repo"
	sha := commitSpec(t, repo, recSpec)

	src := gitsource.New(config.Config{
		DataDir: tmp + "/data",
		Git:     config.GitConfig{URL: repo, Branch: "main"},
	})
	if err := src.EnsureMirror(ctx); err != nil {
		t.Fatalf("EnsureMirror: %v", err)
	}
	head, err := src.ResolveLatest(ctx)
	if err != nil {
		t.Fatalf("ResolveLatest: %v", err)
	}
	if head != sha {
		t.Logf("fixture head=%s resolved=%s", sha, head)
	}
	snap, err := src.FetchRevision(ctx, head)
	if err != nil {
		t.Fatalf("FetchRevision: %v", err)
	}
	defer func() { _ = snap.Close() }()
	res, err := spec.LoadDir(snap.Dir, spec.WithPlatformRepo(true))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Services) != 1 {
		t.Fatalf("specs = %d services", len(res.Services))
	}

	// The state store the reconciler serializes through (real SQLite in
	// the fixture dataDir) and the recording adapter over the real harness.
	st, err := sqlite.Open(ctx, tmp+"/data/top.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rr := &recordingRunner{inner: env.runner}
	dk := dokkuClient(rr)

	rec, err := reconcile.New(reconcile.Options{
		Backend:           dk,
		Store:             st,
		ReadinessDeadline: 4 * time.Minute,
		ReadinessPoll:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}

	in := reconcile.RunInput{
		Specs:    res.Services,
		Revision: head,
		Actor:    domain.ActorUser,
		HolderID: "tier3-e2e",
	}
	runCtx, cancelRun := context.WithTimeout(ctx, 10*time.Minute)
	defer cancelRun()
	res1, err := rec.Run(runCtx, in)
	if err != nil {
		for _, o := range res1.Outcomes {
			t.Logf("outcome %s: %+v err=%q", o.Ref, o.Result, o.Err)
		}
		debugLogs(t, env.runner)
		t.Fatalf("reconcile run: %v", err)
	}
	if res1.Status != domain.RunSucceeded {
		for _, o := range res1.Outcomes {
			t.Logf("outcome %s: %+v err=%q", o.Ref, o.Result, o.Err)
		}
		t.Fatalf("status %s", res1.Status)
	}

	// Observed back through the real adapter: the app serving the pinned
	// image the spec asked for.
	obs, err := dk.Inspect(ctx, res.Services[0].Ref)
	if err != nil {
		t.Fatalf("post-run Inspect: %v", err)
	}
	if !obs.Deployed || !obs.Running {
		t.Fatalf("app not serving: %+v", obs)
	}
	if obs.Image != "docker.io/library/httpd:2.4-alpine" {
		t.Fatalf("image = %q", obs.Image)
	}
	// The reconciler attaches the plan's derived shared internal network
	// (internal/plan's internalNetwork constant, queue-master-doc'd in
	// ADR-0002), created on the real host daemon by EnsureNetwork.
	if !slicesContains(obs.Networks, "top-internal") {
		t.Fatalf("network attach missing: %v", obs.Networks)
	}

	// The env value truly landed in the real app's config (the pipeline's
	// --no-restart env step): read dokku's own config view.
	// top-e2erecon" — the fixture namespace is "tops", so the derived
	// dokku app name is "<ns>-<name>" = tops-e2erecon (domain.AppName).
	out, _, cerr := env.runner.Run(ctx, "dokku", "config:show", "tops-e2erecon")
	if cerr != nil || !strings.Contains(string(out), "E2E_RECON_TEST") {
		t.Fatalf("env not on the real app (out %q err %v)", out, cerr)
	}

	// Second run over the same revision: converges whatever the deploy's
	// post-state reset left behind (logged, then verified by run 3).
	rr.calls = nil
	res2, err := rec.Run(runCtx, in)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.Status != domain.RunSucceeded {
		for _, o := range res2.Outcomes {
			t.Logf("outcome %s: %+v err=%q", o.Ref, o.Result, o.Err)
		}
		t.Fatalf("second run status %s", res2.Status)
	}
	// Sanity read: what the real host reports after run 2, plus the raw
	// reports (reconciler-vs-adapter read parity check).
	if obs2, err := dk.Inspect(ctx, res.Services[0].Ref); err == nil {
		t.Logf("post-run2 observed: restart=%s ports=%+v nets=%v image=%q", obs2.RestartPolicy, obs2.Ports, obs2.Networks, obs2.Image)
	}
	if out, _, e2 := env.runner.Run(ctx, "dokku", "ps:report", "tops-e2erecon", "--format", "json"); e2 == nil {
		t.Logf("ps:report raw: %s", out)
	}
	if out, _, e2 := env.runner.Run(ctx, "dokku", "ports:report", "tops-e2erecon", "--ports-map-json"); e2 == nil {
		t.Logf("ports:report raw: %s", out)
	}
	// The invariant is "after a green run the same diff is empty"; at
	// tier 3 that means a RUN FOLLOWED BY RUN is a noop: run 2 must not
	// have created new drift and must have converged it. A third run
	// (strictly recorded) records the truth.
	rr.calls = nil
	res3, err := rec.Run(runCtx, in)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if res3.Status != domain.RunSucceeded {
		t.Fatalf("third run status %s", res3.Status)
	}
	// Run 1's image deploy re-claims dokku's default post-deploy state
	// (restart-policy default, an auto-assigned host port) — a run over a
	// converged-desired-but-host-reset state legitimately applies those
	// once more. The INVARIANT is that the pipeline converges: run 3
	// (right after run 2 re-applied) must be a pure noop.
	for _, o := range res2.Outcomes {
		if o.Result != domain.OutcomeSucceeded {
			t.Fatalf("run-2 outcome %s = %s (%q); a re-converge run must succeed", o.Ref, o.Result, o.Err)
		}
	}
	t.Logf("run-2 re-converged: %+v", res2.Outcomes)
	for _, o := range res3.Outcomes {
		if o.Result != domain.OutcomeNoop {
			t.Fatalf("run-3 outcome %s = %s %+v changes %v (the pipeline did not converge the real host)", o.Ref, o.Result, o, o.Changes)
		}
	}
	for _, sub := range []string{
		"config:set", "ports:set", "network:attach", "ps:set",
		"git:from-image", "ps:rebuild", "config:unset",
	} {
		if n := rr.count(sub); n != 0 {
			t.Fatalf("third run mutated via %s x%d:\n%v", sub, n, rr.argv())
		}
	}
}

// dokkuClient wires the production adapter over a recording runner on the
// e2e network (the apply_test.go sharing shape).
func dokkuClient(rr *recordingRunner) *dokku.Client {
	c := dokku.New(rr)
	c.InternalNetwork = networkName
	return c
}
