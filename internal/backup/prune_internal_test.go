// SPDX-License-Identifier: MIT

package backup

import (
	"testing"
	"time"

	"github.com/toledompm/top/internal/store"
)

// Pure retention-math boundary tests (white-box: classify is the engine's
// heart and the integrated tests in prune_test.go are end-to-end; these
// pin the exact algorithm contract of prune.go, step by step).

func at(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}

// rows builds a registry-shaped list from NEWEST-first times; paths encode
// the timestamps so tests can match names exactly.
func rows(times ...time.Time) []*store.Backup {
	out := make([]*store.Backup, 0, len(times))
	for _, ts := range times {
		out = append(out, &store.Backup{
			Path:      "/b/top-" + ts.Format("20060102T150405") + ".db",
			CreatedAt: ts,
		})
	}
	return out
}

func pathsOf(rs []*store.Backup) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Path)
	}
	return out
}

func wantPaths(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %v, want %v", i, got, want)
		}
	}
}

func TestClassifyEmptyRegistry(t *testing.T) {
	plan, _ := classify(nil, DefaultPolicy(), time.Now())
	if len(plan.Kept) != 0 || len(plan.Pruned) != 0 {
		t.Fatalf("empty registry plan: %+v", plan)
	}
}

func TestClassifyFreshRegistryKeepsEverything(t *testing.T) {
	now := at(2026, 11, 3, 9)
	rs := rows(now, at(2026, 11, 2, 9), at(2026, 11, 1, 9)) // 3 distinct days
	plan, _ := classify(rs, DefaultPolicy(), now)
	wantPaths(t, pathsOf(plan.Pruned), nil)
	if len(plan.Kept) != 3 {
		t.Fatalf("kept %d, want 3", len(plan.Kept))
	}
}

func TestClassifyDailyBoundary14(t *testing.T) {
	// 20 distinct days, one backup each: 14-daily keeps rows[0..13]; with
	// KeepWeekly=0 the six oldest prune (kept+pruned partition exactly).
	now := at(2026, 11, 3, 9)
	ts := make([]time.Time, 20)
	for i := range ts {
		ts[i] = now.AddDate(0, 0, -i)
	}
	rs := rows(ts...)
	p := Policy{KeepDaily: 14, KeepWeekly: 0}
	plan, _ := classify(rs, p, now)
	if len(plan.Kept) != 14 || len(plan.Pruned) != 6 {
		t.Fatalf("kept %d / pruned %d, want 14/6", len(plan.Kept), len(plan.Pruned))
	}
	wantPaths(t, pathsOf(plan.Kept), pathsOf(rs[:14]))
	wantPaths(t, pathsOf(plan.Pruned), []string{rs[19].Path, rs[18].Path, rs[17].Path, rs[16].Path, rs[15].Path, rs[14].Path}) // oldest first
}

func TestClassifySameDayMultipleOnlyNewestIsDaily(t *testing.T) {
	now := at(2026, 11, 3, 9)
	rs := rows(now, at(2026, 11, 3, 8), at(2026, 11, 3, 7))
	p := Policy{KeepDaily: 5, KeepWeekly: 0}
	plan, _ := classify(rs, p, now)
	wantPaths(t, pathsOf(plan.Kept), pathsOf(rs[:1])) // newest of the day
	wantPaths(t, pathsOf(plan.Pruned), []string{rs[2].Path, rs[1].Path})
}

func TestClassifyWeeklyTakesDistinctFilesAndWeeks(t *testing.T) {
	// KeepDaily=2 keeps the two newest DAYS; the weekly bucket then takes
	// the newest non-daily file of each remaining ISO week (distinct
	// files per class — weekly never re-picks a daily-kept day).
	now := at(2026, 11, 6, 9)
	rs := rows(now, at(2026, 11, 4, 7), at(2026, 10, 27, 7), at(2026, 10, 22, 7), at(2026, 10, 20, 7))
	// Daily: rs0 (Nov6, wk45), rs1 (Nov4, wk45 — different days, both kept).
	// Remainder: rs2 (Oct27, wk44), rs3 (Oct22, wk43), rs4 (Oct20, wk43 dup).
	// Weekly: rs2 (first-seen wk44), rs3 (first-seen wk43); rs4 prunes.
	p := Policy{KeepDaily: 2, KeepWeekly: 2}
	plan, _ := classify(rs, p, now)
	wantPaths(t, pathsOf(plan.Kept), pathsOf(rs[:4]))
	wantPaths(t, pathsOf(plan.Pruned), []string{rs[4].Path})
}

func TestClassifyMaxAgeDemotesButNewestSurvives(t *testing.T) {
	now := at(2026, 11, 3, 9)
	rs := rows(now, at(2026, 11, 2, 9), at(2026, 11, 2, 8))
	p := Policy{KeepDaily: 14, KeepWeekly: 8, MaxAge: 6 * time.Hour}
	plan, _ := classify(rs, p, now)
	// Both 11-02 backups are older than the cutoff (11-03 03:00); the
	// newest of today survives even though MaxAge would starve the plan.
	wantPaths(t, pathsOf(plan.Kept), pathsOf(rs[:1]))
	wantPaths(t, pathsOf(plan.Pruned), []string{rs[2].Path, rs[1].Path}) // oldest first
}

func TestClassifyMaxAgeNeverStarvesSingleBackup(t *testing.T) {
	now := at(2026, 11, 3, 9)
	rs := rows(at(2026, 10, 1, 9))
	p := Policy{KeepDaily: 14, KeepWeekly: 8, MaxAge: time.Hour}
	plan, _ := classify(rs, p, now)
	if len(plan.Kept) != 1 || len(plan.Pruned) != 0 {
		t.Fatalf("max-age starvation guard: %+v", plan)
	}
}

func TestClassifyPrunedIsOldestFirst(t *testing.T) {
	now := at(2026, 11, 3, 9)
	rs := rows(now, at(2026, 10, 21, 9), at(2026, 10, 20, 9))
	p := Policy{KeepDaily: 1, KeepWeekly: 0}
	plan, _ := classify(rs, p, now)
	wantPaths(t, pathsOf(plan.Pruned), []string{rs[2].Path, rs[1].Path})
}
