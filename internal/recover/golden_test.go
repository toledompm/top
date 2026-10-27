// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/exec/exectest"
)

// The golden test pins the backup-recovery contract's example output
// shape for `topctl recover diagnose` — the controller-dead + disk-full
// fixture — against internal/recover/testdata/diagnose.golden.
//
// Golden-shape parity note (task point: contract example).
// docs/contracts/backup-recovery.md's illustrative console block shows:
//
//	✓ dokku 0.38.31 healthy
//	✗ core-top-controller: container exited (1), last deploy 3h ago
//	  → last log lines suggest: cannot open /var/lib/top/top.db: disk I/O error
//	  → suggested: check disk space …; then `topctl recover restart`
//	✓ sqlite: /var/lib/top/top.db integrity ok (but stale: newest op 41h old)
//
// The doctor Finding/Report shape carries the same fact structure (one
// row per check: verdict + subject + detail; non-ok rows carry a
// "→ suggested: …" action), while sharing the EXACT vocabulary with
// `topctl doctor` (same Finding fields, severities, and alignment) so
// agents read one report format across topctl. Differences, recorded:
// doctor's `✓/✗` glyphs vs our severity word column (env/agent-friendly,
// grep-stable), and absolute ages are printed from the test clock
// rather than round prose ("41h0m0s" ↔ "41h old").
func TestGoldenDiagnoseControllerDeadDiskFull(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "top.db")

	// The store: migrated, with one op finished exactly 41h before our
	// fixed recovery clock (the contract example's "41h old").
	fixed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	writeOldOp(t, db, fixed.Add(-41*time.Hour))

	// The dokku state: healthy CLI, app exists, deployed-but-not-running,
	// logs blaming disk I/O on top.db, no failed-deploy log.
	runner := exectest.NewFakeRunner()
	runner.SetOutput("dokku", okResp("dokku version 0.38.31 (git commit abc)"), "version")
	runner.SetOutput("dokku", okResp("=====> core-top-controller\n=====> core-web"), "apps:list")
	runner.SetOutput("dokku", okResp(reportJSON(true, false)), "ps:report", controllerAppFixture, "--format", "json")
	runner.SetOutput("dokku",
		okResp("top-controller[0]: started\ntop-controller[1]: cannot open "+dir+"/top.db: disk I/O error\n"),
		"logs", controllerAppFixture, "--num", "200")
	runner.SetDefault(okResp(""))

	opts := Options{
		Runner:         runner,
		DataDir:        dir,
		ConfigFile:     dir + "/top-config.yaml",
		BootstrapImage: "ghcr.io/example/top/topctl:bootstrap-test",
		DiskProbe:      func(string) (uint64, error) { return 0, nil }, // disk FULL
		Now:            func() time.Time { return fixed },
	}
	rep := Diagnose(context.Background(), opts)
	if rep.OK() {
		t.Fatalf("the fixture must produce failures: %+v", rep.Findings)
	}

	// Sanity: the contract's fact chain is present…
	byID := map[string]Finding{}
	for _, f := range rep.Findings {
		byID[f.ID] = f
	}
	for _, id := range []string{"dokku/health", "controller/state", "controller/log-tail", "store/integrity", "disk/headroom"} {
		if _, has := byID[id]; !has {
			t.Fatalf("golden fixture lost finding %q: %+v", id, byID)
		}
	}
	if !strings.Contains(byID["controller/log-tail"].Message, "disk I/O error") {
		t.Errorf("log evidence missing: %+v", byID["controller/log-tail"])
	}

	// …and the rendered text matches the golden file byte for byte.
	// Paths inside a t.TempDir() vary per run, so the golden file carries
	// the TMPDIR marker; substitute it for comparison.
	var b strings.Builder
	if err := RenderText(&b, rep); err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(b.String(), dir, "TMPDIR")
	want, err := os.ReadFile(filepath.Join("testdata", "diagnose.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		if os.Getenv("GOLDEN_UPDATE") != "" {
			if err := os.WriteFile(filepath.Join("testdata", "diagnose.golden"), []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Log("golden updated")
			return
		}
		t.Fatalf("golden mismatch (run with GOLDEN_UPDATE=1 to refresh):\n--- GOT ---\n%s\n--- WANT ---\n%s", got, string(want))
	}
}
