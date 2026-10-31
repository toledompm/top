// SPDX-License-Identifier: MIT

package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/toledompm/top/internal/exec/exectest"
)

// The M3 milestone's CI gate: the idempotency invariant over `topctl` —
//
//	diff exits 1 with field-level drift
//	  → `topctl reconcile` runs AND converges
//	    → the same diff exits 0 (drift gone, ids 1→run→0)
//
// One fixture, two phases, one FakeRunner simulating a real Dokku host
// whose state is advanced by the run itself: the fake's apps:exists
// answers "missing" exactly once (drift world), then "exists"; the
// report answers are the CONVERGED state for the fixture spec — which is
// precisely what a real dokku reports after the appliers' commands
// (config:set/ports:set/…, which the fake accepts as no-ops) executed.
// A afterward-run diff therefore reads the same reports the real host
// would show post-converge: POST-converge parity, not a lying fake.
//
// Fixture seam: recon_test.go's split-runner (real git for the local
// platform-repo remote, FakeRunner for everything dokku-shaped); no
// network, no host mutations.

// recConvergedReports seeds every inspect report with the converged state
// for the webSpec fixture: serving web on the pinned image everywhere,
// exactly the config keys/network/ports the appliers would have set.
func recConvergedReports(f *exectest.FakeRunner) {
	setPrefix := func(sub []string, out string) {
		f.SetPrefix(append([]string{"dokku"}, sub...), exectest.Response{Stdout: []byte(out)})
	}
	setPrefix([]string{"ps:report"}, `{"deployed":true,"running":true,"computed-restart-policy":"always"}`)
	setPrefix([]string{"builds:report"}, `{"build-status":"succeeded"}`)
	setPrefix([]string{"git:report"}, `{"source-image":"ghcr.io/example/web:1.2.3"}`)
	setPrefix([]string{"apps:report"}, `{"deploy-source":"docker-image","deploy-source-metadata":"ghcr.io/example/web:1.2.3"}`)
	setPrefix([]string{"network:report"}, `{"computed-attach-post-deploy":"top-internal"}`)
	setPrefix([]string{"resource:report"}, `{}`)
	setPrefix([]string{"ports:report"}, `[{"scheme":"http","host_port":0,"container_port":8080}]`)
	setPrefix([]string{"storage:list"}, `[]`)
	setPrefix([]string{"config:keys"}, "TZ\n")
	// Everything the appliers run (apps:create, git:from-image,
	// config:set, ports:set, network:*, ps:set) succeeds silently.
	f.SetDefault(exectest.Response{})
}

// TestReconcileDriftThenEmptyDiff runs the drift→reconcile→empty-diff
// invariant end to end at the fake tier (the milestone's CI gate).
func TestReconcileDriftThenEmptyDiff(t *testing.T) {
	t.Parallel()
	fx := newReconFixture(t, true)
	// Phase 0 (drift world): the app does not exist yet. apps:exists
	// answers "missing" for the first call only — every later call hits
	// the converged tail the run itself "created".
	fx.f.SetScriptedPrefix([]string{"dokku", "apps:exists"},
		[]exectest.Response{{ExitCode: 20, Stderr: []byte("!  App top-web does not exist")}},
		exectest.Response{})
	recConvergedReports(fx.f)

	// Phase 1: diff with drift → exit 1 with a field-level create row.
	outS, errS, code := fx.run(t, "diff", "--revision", fx.repoHEAD, "-o", "json")
	if code != ExitDrift {
		t.Fatalf("pre-run diff exit = %d (err %q)", code, errS)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("diff json: %q: %v", outS, err)
	}
	rows, _ := env.Data["services"].([]any)
	if len(rows) != 1 {
		t.Fatalf("drift rows = %v", env.Data["services"])
	}
	if row, _ := rows[0].(map[string]any); row["service"] != "tops/web" || row["change"] != "create" {
		t.Fatalf("row = %v", rows[0])
	}

	// Phase 2: reconcile over the SAME revision → run succeeds.
	fx.openStore(t)                           // the store must exist before a REAL reconcile (reads never invent it)
	outS, errS, code = fx.run(t, "reconcile") // converge at resolved HEAD (== repoHEAD)
	if code != ExitOK {
		t.Fatalf("reconcile exit = %d (out %q err %q)", code, outS, errS)
	}

	// Phase 3: the SAME diff command now exits 0 with zero rows — the
	// invariant (cli.md contract anchor: "after a green run the same diff
	// exits 0").
	outS, errS, code = fx.run(t, "diff", "--revision", fx.repoHEAD, "-o", "json")
	if code != ExitOK {
		t.Fatalf("post-run diff exit = %d (out %q err %q)", code, outS, errS)
	}
	if err := json.Unmarshal([]byte(outS), &env); err != nil {
		t.Fatalf("diff json: %q: %v", outS, err)
	}
	// The converged truth renders at most a fieldless noop row — NOT
	// drift (contract anchor: "after a green run the same diff exits 0").
	for _, r0 := range env.Data["services"].([]any) {
		row := r0.(map[string]any)
		fields := len(row["fields"].([]any))
		if row["change"] != "noop" || fields != 0 {
			t.Fatalf("invariant violated — post-run diff reports drift: %v", env.Data["services"])
		}
	}
	if strings.Contains(string(outS), "delete(") {
		t.Fatalf("invariant violated — post-run diff reports deletes: %s", outS)
	}
}
