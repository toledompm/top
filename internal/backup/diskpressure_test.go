package backup_test

// SPDX-License-Identifier: MIT

// T-504 scenario 3 (failure-injection matrix "Disk pressure") — the
// hermetic shard. The tier-4 script (test/e2e/t504-disk-pressure.sh)
// drives the HOST posture (real ENOSPC, disk/headroom diagnose
// findings); the quota-free slice lives here, against the engine:
//
//   - Create under a doomed destination fails BOUNDED: one error, a
//     `result=error` metric, and NO registry row — "no silent corruption"
//     (the matrix's wording). The doomed-destination proxy is a
//     mode-0555 backup dir: every write the Create window needs is
//     refused by the OS for a deterministic error. (A real ENOSPC needs
//     no extra code: VACUUM INTO hits the same refusal path —
//     backup.go's `vacuum into %s: %w` wrap — with the same bounding.)
//   - Plan still works under the wedge: retention computation reads the
//     registry only and must not be starved by the destination's state.
//   - A stale backup is still PRUNABLE: once capacity is restored
//     (0555 → 0750), the prune deletes the stale file then its registry
//     row — deletions are the one op a genuinely full disk does not
//     block, and the engine's posture (refuse bounded under the proxy)
//     plus the post-lift success are both pinned here.
//
// Additions on top of the T-501/T-502 tests already in this package
// (engine_test.go, prune_test.go); they share its harness (testEnv /
// fakeRecorder / insertOp).

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backup"
	"github.com/toledompm/top/internal/store"
)

// wedgeDir makes the engine's backup destination refuse every create,
// restored on test cleanup (0o750, the engine's on-demand mode).
func wedgeDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod 0555 %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
}

// twoBackupsEightDaysApart creates one backup, advances the fake clock
// past a retention day boundary, and creates a second (newer) one. The
// FIRST is the retention-stale one (KeepDaily=1 keeps only the second's
// day).
func twoBackupsEightDaysApart(t *testing.T, env *testEnv) (stale, fresh *store.Backup) {
	t.Helper()
	insertOp(t, env.store) // watermark source
	first := mustCreate(t, env.engine)
	env.step(8 * 24 * time.Hour)
	second := mustCreate(t, env.engine)
	return first, second
}

func TestEngineCreateDiskPressureRefusal(t *testing.T) {
	env := newEnv(t)

	// one GOOD first so the wedge is proven not to retroactively damage
	// anything on disk or in the registry.
	good := mustCreate(t, env.engine)

	wedgeDir(t, env.dir)

	start := time.Now()
	b, err := env.engine.Create(context.Background())
	if err == nil {
		t.Fatalf("Create succeeded over a refused destination: %+v", b)
	}
	if b != nil {
		t.Errorf("Create returned a row alongside its error (nil-on-failure contract): %+v", b)
	}
	if dur := time.Since(start); dur > 30*time.Second {
		t.Errorf("Create took %v to refuse — the refusal must be bounded, not a hang", dur)
	}
	// bounded diagnostics: the error must name the engine stage and the
	// doomed target, not leak a bare syscall line.
	if !strings.Contains(err.Error(), "vacuum into") ||
		!strings.Contains(err.Error(), env.dir) {
		t.Errorf("error lost its engine lineage/target: %v", err)
	}
	// metric posture: every refusal lands as result=error (the earlier
	// good create's ok result stays — history, not overwrite).
	got := env.rec.results
	if len(got) == 0 || got[len(got)-1] != backup.ResultError {
		t.Errorf("metric results %v: the refusal must land as [%s]", got, backup.ResultError)
	}
	// no silent corruption: the good backup file + its row survive, and
	// the refusal inserted NO row.
	rows, lerr := env.store.ListBackups(context.Background())
	if lerr != nil {
		t.Fatalf("ListBackups: %v", lerr)
	}
	if len(rows) != 1 || rows[0].Path != good.Path {
		t.Fatalf("registry after refusal = %d rows (first %+v); want exactly the good backup", len(rows), rows)
	}
}

func TestEnginePlanWorksUnderWedge(t *testing.T) {
	env := newEnv(t)
	stale, fresh := twoBackupsEightDaysApart(t, env)

	wedgeDir(t, env.dir)

	plan, err := env.engine.Plan(context.Background(), backup.Policy{KeepDaily: 1})
	if err != nil {
		t.Fatalf("Plan under wedge: %v", err)
	}
	if plan.Empty() {
		t.Fatalf("Plan kept nothing with KeepDaily=1 (retention math broke under the wedge)")
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Path != stale.Path {
		t.Errorf("prune target = %+v, want exactly the older backup %s", plan.Pruned, stale.Path)
	}
	if len(plan.Kept) != 1 || plan.Kept[0].Path != fresh.Path {
		t.Errorf("keeper = %+v, want exactly the newer backup %s", plan.Kept, fresh.Path)
	}
}

func TestStalePrunableAfterPressureLift(t *testing.T) {
	env := newEnv(t)
	stale, _ := twoBackupsEightDaysApart(t, env)

	// Under the 0555 proxy wedge Prune refuses bounded (it unlinks; the
	// OS refuses arbitrarily) — the honest engine posture. After
	// capacity is restored the stale set goes: file, then row (the
	// engine's deletion order, prune.go).
	wedgeDir(t, env.dir)
	if _, err := env.engine.Prune(context.Background(), backup.Policy{KeepDaily: 1}); err == nil {
		t.Fatal("Prune through a 0555 dir must refuse (it unlinks); got nil error")
	}
	if err := os.Chmod(env.dir, 0o750); err != nil {
		t.Fatalf("un-wedge: %v", err)
	}
	plan, err := env.engine.Prune(context.Background(), backup.Policy{KeepDaily: 1})
	if err != nil {
		t.Fatalf("Prune after lift: %v", err)
	}
	if len(plan.Pruned) != 1 || plan.Pruned[0].Path != stale.Path {
		t.Fatalf("Pruned = %+v, want exactly the stale backup %s", plan.Pruned, stale.Path)
	}
	if _, serr := os.Stat(stale.Path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("stale file survived the prune: %v", serr)
	}
	rows, lerr := env.store.ListBackups(context.Background())
	if lerr != nil {
		t.Fatalf("ListBackups: %v", lerr)
	}
	for _, r := range rows {
		if r.Path == stale.Path {
			t.Errorf("stale ROW survived the prune (file-first, row-second order violated)")
		}
	}
}
