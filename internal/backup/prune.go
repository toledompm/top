// SPDX-License-Identifier: MIT

package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// Policy is the retention configuration (ADR-0007 "Retention: keep daily
// for 14 days + weekly for 8 weeks (configurable)"; the contract's
// `Prune(ctx, policy)` parameter — the engine owns the mechanism, the
// caller owns the choice and the consent).
type Policy struct {
	// KeepDaily keeps the newest KeepDaily DISTINCT UTC calendar days,
	// one backup per day (its newest that day).
	KeepDaily int
	// KeepWeekly keeps the newest KeepWeekly DISTINCT ISO weeks from the
	// backups not already kept as dailies (their newest that week).
	KeepWeekly int
	// MaxAge drops anything older than now-MaxAge that is not the newest
	// backup (0 = disabled). Compatibility valve for "also nothing older
	// than N" operators; the daily/weekly counts above are the primary
	// policy per ADR-0007.
	MaxAge time.Duration
}

// DefaultPolicy is ADR-0007's verbatim default: 14 daily + 8 weekly.
func DefaultPolicy() Policy { return Policy{KeepDaily: 14, KeepWeekly: 8} }

// Validate rejects impossible policies (negative counts; a MaxAge that
// alone would delete the newest backup is allowed — newest never dies).
func (p Policy) Validate() error {
	if p.KeepDaily < 0 {
		return fmt.Errorf("retention policy: %w: negative KeepDaily", domain.ErrInvalid)
	}
	if p.KeepWeekly < 0 {
		return fmt.Errorf("retention policy: %w: negative KeepWeekly", domain.ErrInvalid)
	}
	if p.KeepDaily == 0 && p.KeepWeekly == 0 && p.MaxAge == 0 {
		return fmt.Errorf("retention policy: %w: empty policy keeps nothing; use DefaultPolicy() (14 daily + 8 weekly per ADR-0007)", domain.ErrInvalid)
	}
	return nil
}

// Plan is a Prune script: what survives, what goes. Plan is the consent
// artifact — the caller renders it before executing (BackupProvider's
// `Prune` returns `deleted []Backup`; Plan names them BEFORE deletion, so
// T-503's CLI can show→confirm→delete in one engine round-trip).
type Plan struct {
	// Policy the plan was computed with.
	Policy Policy
	// Kept is the surviving set, newest first.
	Kept []*store.Backup
	// Pruned is the to-delete set, oldest first (deletion order).
	Pruned []*store.Backup
}

// Empty reports whether the plan deletes nothing (callers can skip the
// confirmation round entirely).
func (p *Plan) Empty() bool { return len(p.Pruned) == 0 }

// plan (engine-internal, non-validating) computes the keep/prune script.
func (e *Engine) plan(ctx context.Context, p Policy) (*Plan, error) {
	rows, err := e.Registry.ListBackups(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup plan: registry: %w", err)
	}
	plan, why := classify(rows, p, e.clock())
	e.Log.Info("backup plan computed",
		"kept", len(plan.Kept), "pruned", len(plan.Pruned), "why", why)
	return plan, nil
}

// classify is the retention math, pure so the policy-boundary tests can be
// hermetic and exact. Input is the registry newest-first (as
// Registry.ListBackups returns; rows[0] is the newest backup).
//
// Retention algorithm (this package's documented contract):
//
//  1. Daily pass, newest→oldest: the FIRST-SEEN (newest) backup of each
//     distinct UTC calendar day is a daily candidate; the newest
//     KeepDaily candidates survive.
//  2. Weekly pass over the backups NOT kept as dailies: the first-seen
//     (newest) backup of each distinct ISO week is a weekly candidate;
//     the newest KeepWeekly survive. (A weekly candidate is never the
//     backup already kept for a day, so grand-father-father-son keeps
//     distinct files per class.)
//  3. MaxAge valve: when enabled, kept backups older than now-MaxAge are
//     demoted to pruned — the newest backup ALWAYS survives (rule 4) and
//     MaxAge can therefore never leave the operator with no backups.
//  4. Everything else is pruned, in oldest-first order (deletion order).
//     Total survivors ≤ KeepDaily + KeepWeekly, and never zero.
//
// Emitted in *Plan (the consent artifact; Prune applies it).
func classify(rows []*store.Backup, p Policy, now time.Time) (*Plan, string) {
	plan := &Plan{Policy: p}
	if len(rows) == 0 {
		return plan, "registry empty"
	}

	kept := make([]bool, len(rows))
	// Rule 1-2, dailies: newest per distinct day.
	daySeen := map[string]bool{}
	daily := 0
	for i, b := range rows {
		day := b.CreatedAt.Format("2006-01-02")
		if daySeen[day] {
			continue
		}
		daySeen[day] = true
		if daily < p.KeepDaily {
			daily++
			kept[i] = true
		}
	}
	// Rule 2, weeklies: newest per distinct ISO week among the remainder.
	weekSeen := map[weekKey]bool{}
	weekly := 0
	for i, b := range rows {
		if kept[i] {
			continue // weekly class takes distinct files from the non-daily set
		}
		wk := weekOf(b.CreatedAt)
		if weekSeen[wk] {
			continue
		}
		weekSeen[wk] = true
		if weekly < p.KeepWeekly {
			weekly++
			kept[i] = true
		}
	}
	// Rule 3: MaxAge demotion, newest always survives.
	var cutoff time.Time
	if p.MaxAge > 0 {
		cutoff = now.Add(-p.MaxAge)
		for i, b := range rows {
			if i == 0 || !kept[i] || !b.CreatedAt.Before(cutoff) {
				continue
			}
			kept[i] = false
		}
	}
	// Never zero survivors (belt and braces: rule 3 already protects rows[0]).
	kept[0] = true

	for i, b := range rows {
		switch {
		case kept[i]:
			plan.Kept = append(plan.Kept, b)
		default:
			plan.Pruned = append([]*store.Backup{b}, plan.Pruned...) // oldest first
		}
	}
	return plan, "retention script"
}

// weekKey is (ISO year, ISO week).
type weekKey struct{ y, w int }

// weekOf classifies a timestamp into its ISO week.
func weekOf(t time.Time) weekKey {
	y, w := t.ISOWeek()
	return weekKey{y, w}
}
