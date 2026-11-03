package install

// The ledger-aware preflight overlay (tier-4 verified 2026-11-03): a
// TOP-owned controller row must not abort an idempotent `topctl install`
// (installation.md's "re-running … resumes or skips" promise). Foreign and
// pre-existing rows keep the hard failure.
import (
	"context"
	"testing"
	"time"

	"github.com/toledompm/top/internal/doctor"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

func seedReport() *doctor.Report {
	return &doctor.Report{Findings: []doctor.Finding{{
		ID:       "names/top-controller",
		Severity: doctor.SeverityFail,
		Check:    "reserved names",
		Message:  `a Dokku app named "core-top-controller" already exists on this host`,
	}}}
}

func seedControllerLedgerRow(t *testing.T, st store.Store, markDomain string) {
	t.Helper()
	if err := inventory.MarkCreated(context.Background(), st, inventory.KindApp, doctor.ControllerAppName, "op_test", nil, time.Now()); err != nil {
		t.Fatalf("mark ledger row: %v", err)
	}
	if markDomain != "" {
		_ = markDomain // state flipping beyond bootstrap-owned is testFuture
	}
}

func TestPreflightDowngradesLedgeredControllerName(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		state int // 1 = bootstrap-owned (issue-state), 2 = managed
		want  doctor.Severity
	}{
		{"bootstrap-owned row downgrades to info", 1, doctor.SeverityInfo},
		{"managed row downgrades to info", 2, doctor.SeverityInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			seedControllerLedgerRow(t, st, "")
			if tc.state == 2 {
				if err := st.MarkManaged(ctx, inventory.KindApp, doctor.ControllerAppName); err != nil {
					t.Fatalf("MarkManaged: %v", err)
				}
			}
			rep := seedReport()
			downgradeLedgeredControllerName(ctx, st, rep)
			if !rep.OK() {
				t.Fatalf("preflight must pass after the downgrade: %+v", rep.Findings)
			}
			got := rep.Findings[0].Severity
			if got != doctor.SeverityInfo {
				t.Fatalf("severity = %q, want info (a warn would still read as user-visible action)", got)
			}
		})
	}
}

func TestPreflightKeepsForeignControllerCollision(t *testing.T) {
	ctx := context.Background()

	t.Run("no ledger row at all", func(t *testing.T) {
		st := testStore(t)
		rep := seedReport()
		downgradeLedgeredControllerName(ctx, st, rep)
		if rep.Findings[0].Severity != doctor.SeverityFail {
			t.Fatalf("a foreign app must keep the hard failure: %+v", rep.Findings[0])
		}
	})

	t.Run("pre-existing row never converts (ADR-0009)", func(t *testing.T) {
		st := testStore(t)
		row := &store.Resource{
			Kind: inventory.KindApp, Identifier: doctor.ControllerAppName,
			State: store.ResourcePreExisting,
		}
		if err := st.InsertLedgerEntry(ctx, row); err != nil {
			t.Fatalf("insert pre-existing row: %v", err)
		}
		rep := seedReport()
		downgradeLedgeredControllerName(ctx, st, rep)
		if rep.Findings[0].Severity != doctor.SeverityFail {
			t.Fatalf("pre-existing row must keep the hard failure: %+v", rep.Findings[0])
		}
	})
}
