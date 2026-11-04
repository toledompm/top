// SPDX-License-Identifier: MIT

package backup_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backup"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// Engine-integrated prune tests (the pure policy math lives in
// prune_internal_test.go, white-box). Prune is DESTRUCTIVE — these tests
// prove it deletes files AND registry rows exactly per the plan, is
// idempotent, and never touches what Plan promised to keep.

func dayAt(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}

// seed registers a placeholder backup file + registry row. CreatedAt comes
// from the caller (distinct times give the registry's newest-first order).
func seed(t *testing.T, env *testEnv, name string, at time.Time) *store.Backup {
	t.Helper()
	if err := os.MkdirAll(env.dir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(env.dir, name)
	if err := os.WriteFile(target, []byte("placeholder backup bytes"), 0o600); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
	b := &store.Backup{Path: target, Size: 24, WatermarkOp: "", CreatedAt: at.UTC()}
	if err := env.store.InsertBackup(context.Background(), b); err != nil {
		t.Fatalf("seed insert %s: %v", name, err)
	}
	return b
}

func namesOf(rows []*store.Backup) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, filepath.Base(r.Path))
	}
	return strings.Join(out, ",")
}

func TestEnginePlanChangesNothing(t *testing.T) {
	env := newEnv(t)
	keep := seed(t, env, "a.db", dayAt(2026, 11, 3, 9))
	dead := seed(t, env, "b.db", dayAt(2026, 11, 2, 9))

	plan, err := env.engine.Plan(context.Background(), backup.Policy{KeepDaily: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := namesOf(plan.Pruned); got != "b.db" {
		t.Fatalf("plan pruned %q, want b.db", got)
	}
	for _, want := range []string{keep.Path, dead.Path} {
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("Plan deleted %s: Plan must not mutate", want)
		}
		if got, _ := env.store.ListBackups(context.Background()); len(got) != 2 {
			t.Fatalf("Plan removed registry rows: %+v", got)
		}
	}
}

func TestEnginePruneAppliesAndIsIdempotent(t *testing.T) {
	env := newEnv(t)
	keep := seed(t, env, "a.db", dayAt(2026, 11, 3, 9))
	dead1 := seed(t, env, "b.db", dayAt(2026, 11, 2, 8))
	dead2 := seed(t, env, "c.db", dayAt(2026, 11, 2, 7))
	_ = dead2

	plan, err := env.engine.Prune(context.Background(), backup.Policy{KeepDaily: 1})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := namesOf(plan.Pruned); got != "c.db,b.db" { // oldest first
		t.Fatalf("pruned %q, want c.db,b.db", got)
	}
	if _, err := os.Stat(dead1.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file %s not deleted", dead1.Path)
	}
	if _, err := os.Stat(keep.Path); err != nil {
		t.Fatalf("survivor deleted: %v", err)
	}
	got, _ := env.store.ListBackups(context.Background())
	if len(got) != 1 || got[0].Path != keep.Path {
		t.Fatalf("registry after prune: %+v", got)
	}

	// Idempotent: re-run is a no-op — no rows, no files, no error.
	plan2, err := env.engine.Prune(context.Background(), backup.Policy{KeepDaily: 1})
	if err != nil || !plan2.Empty() {
		t.Fatalf("second prune = %+v, %v; want empty no-op", plan2, err)
	}
}

func TestEnginePruneMissingFileStillDeregisters(t *testing.T) {
	// A registry row without its file is a lie Prune can fix: deletion of
	// an already-missing file counts as done, the row still goes.
	env := newEnv(t)
	ghost := seed(t, env, "ghost.db", dayAt(2026, 11, 2, 9))
	seed(t, env, "fresh.db", dayAt(2026, 11, 3, 9))
	if err := os.Remove(ghost.Path); err != nil {
		t.Fatal(err)
	}
	plan, err := env.engine.Prune(context.Background(), backup.Policy{KeepDaily: 1})
	if err != nil {
		t.Fatalf("Prune over pre-missing file: %v", err)
	}
	if len(plan.Pruned) != 1 {
		t.Fatalf("expected the ghost pruned: %+v", plan)
	}
	if got, _ := env.store.ListBackups(context.Background()); len(got) != 1 {
		t.Fatalf("registry did not clear the ghost row: %+v", got)
	}
}

func TestEnginePruneInvalidPolicyRefusedNothingDeleted(t *testing.T) {
	env := newEnv(t)
	seed(t, env, "a.db", dayAt(2026, 11, 3, 9))
	if _, err := env.engine.Prune(context.Background(), backup.Policy{}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("empty-policy prune err = %v, want ErrInvalid", err)
	}
	if got, _ := env.store.ListBackups(context.Background()); len(got) != 1 {
		t.Fatalf("failed prune deleted something: %+v", got)
	}
}

// TestEnginePruneGrandfatherPattern exercises ADR-0007's default policy
// (14 daily + 8 weekly) on a hand-computed registry: 16 distinct days
// (daily keeps the 14 newest days) plus non-daily rows in several ISO
// weeks (weekly keeps one per week). Exact survivors asserted.
func TestEnginePruneGrandfatherPattern(t *testing.T) {
	if dp := backup.DefaultPolicy(); dp.KeepDaily != 14 || dp.KeepWeekly != 8 {
		t.Fatalf("DefaultPolicy = %+v, want 14 daily + 8 weekly (ADR-0007)", dp)
	}
	env := newEnv(t)
	// Newest-first registry seeding times; distinct days r0..r15.
	times := []struct {
		name string
		at   time.Time
	}{}
	base := dayAt(2026, 11, 3, 9)
	for i := 0; i < 16; i++ {
		times = append(times, struct {
			name string
			at   time.Time
		}{fmt.Sprintf("r%d.db", i), base.AddDate(0, 0, -i)})
	}
	// Remainder rows: duplicates back into older ISO weeks.
	for _, x := range []struct {
		n  string
		at time.Time
	}{
		{"r16.db", dayAt(2026, 10, 13, 9)}, // ISO wk42, no daily keep
		{"r17.db", dayAt(2026, 10, 6, 9)},  // ISO wk41
		{"r18.db", dayAt(2026, 10, 12, 9)}, // ISO wk42 again (same week as r16)
		{"r19.db", dayAt(2026, 10, 1, 9)},  // ISO wk40
	} {
		times = append(times, struct {
			name string
			at   time.Time
		}{x.n, x.at})
	}
	for _, x := range times {
		seed(t, env, x.name, x.at)
	}

	plan, err := env.engine.Plan(context.Background(), backup.DefaultPolicy())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := namesOf(plan.Kept); got != "r0.db,r1.db,r2.db,r3.db,r4.db,r5.db,"+
		"r6.db,r7.db,r8.db,r9.db,r10.db,r11.db,r12.db,r13.db,"+
		"r14.db,r16.db,r17.db,r19.db" {
		t.Fatalf("kept = %q (daily r0..r13 + weekly r14,r16,r17,r19)", got)
	}
	if got := namesOf(plan.Pruned); got != "r18.db,r15.db" { // oldest first
		t.Fatalf("pruned = %q, want r18,r15 (wk42 dup of r16 kept, r15 wk43 dup of r14 kept)", got)
	}
	if plan.Empty() {
		t.Fatal("sanity: plan unexpectedly empty")
	}
	// Prune executes the same script and is end-state-equal to the plan.
	final, err := env.engine.Prune(context.Background(), backup.DefaultPolicy())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := namesOf(final.Kept); got != namesOf(plan.Kept) {
		t.Fatalf("prune kept %q, plan kept %q", got, namesOf(plan.Kept))
	}
}

func TestEnginePruneMaxAgeNeverLeavesEmpty(t *testing.T) {
	// MaxAge would delete EVERYTHING (all backups older than 1h): the
	// newest must still survive (retention never ends with zero backups).
	env := newEnv(t)
	seed(t, env, "old.db", dayAt(2026, 10, 1, 9))
	p := backup.Policy{KeepDaily: 14, KeepWeekly: 8, MaxAge: time.Hour}
	plan, err := env.engine.Prune(context.Background(), p)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if got := len(plan.Kept); got != 1 || len(plan.Pruned) != 0 {
		t.Fatalf("max-age starvation guard failed: kept %d pruned %d", got, len(plan.Pruned))
	}
}
