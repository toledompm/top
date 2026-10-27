// SPDX-License-Identifier: MIT
package inventory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

var (
	nsNames = []string{"core", "observability", "default"}
	now     = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
)

func openLedgerStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "top.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// This file uses the fixture helpers from snapshot_test.go (the real
// exectest fake) plus a temp-file SQLite store: no host mutation, no
// network, tier-1 hermetic.

// TestAdoptableTruthTable walks the ADR-0009 adoption matrix against the
// fixture snapshot: label match, ledger-independent presence, name
// mismatch, label mismatch, and the namespace naming rules.
func TestAdoptableTruthTable(t *testing.T) {
	f := fixture(t)
	snap, err := Snapshot(context.Background(), f, testOpts())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	cases := []struct {
		name    string
		kind    string
		ident   string
		want    bool
		wantWhy Reason
	}{
		// Layer-2 label match: core-top-controller carries the managed label.
		{"label match", KindContainer, "core-top-controller", true, ReasonOK},
		// Layer-1 naming match only: default-website is a Dokku app and
		// carries a registered namespace prefix on the app kind.
		{"naming match (dokku app)", KindApp, "default-website", true, ReasonOK},
		// Label mismatch (no managed label at all) → foreign. Present in
		// the fresh snapshot, so key 2 can actually evaluate.
		{"label mismatch", KindContainer, "redis-cache", false, ReasonRefusedForeignIdentifier},
		// Name mismatch with presence: foreign container name evaluated
		// live. (App-kind name mismatches that are entirely absent from
		// the snapshot are caught by the absent check first — see
		// NameOwnedBy unit tests for the naming rule itself.)
		{"name mismatch (present)", KindContainer, "redis-cache", false, ReasonRefusedForeignIdentifier},
		{"name mismatch (absent)", KindApp, "somebody-elses-app", false, ReasonAbsentSnapshot},
		{"name mismatch (absent, container)", KindContainer, "somebody-elses-app", false, ReasonAbsentSnapshot},
		// Core-namespace naming edge: prefix-only identifiers must not
		// count as owned (rule unit-tested below); at snapshot level the
		// absent check wins.
		{"core ns prefix only (absent)", KindApp, "core-", false, ReasonAbsentSnapshot},
		{"core ns", KindApp, "core-top-controller", true, ReasonOK},
		// Not in the snapshot at all → absent, not adoptable.
		{"absent from snapshot", KindApp, "default-ghost", false, ReasonAbsentSnapshot},
		// Unknown ledger kind never enters the rules.
		{"unknown kind", "image", "whatever", false, ReasonRefusedForeignIdentifier},
	}
	for _, tc := range cases {
		d := Adoptable(tc.kind, tc.ident, snap, nsNames)
		if d.Adoptable != tc.want || d.Reason != tc.wantWhy {
			t.Errorf("%s: Adoptable(%s, %q) = %+v, want adoptable=%v reason=%v",
				tc.name, tc.kind, tc.ident, d, tc.want, tc.wantWhy)
		}
	}
}

// TestAdoptableNetworkVolume: layer-2 resources adopt only on the label
// (naming agreement never reaches docker-level identifiers).
func TestAdoptableNetworkVolume(t *testing.T) {
	f := fixture(t)
	snap, err := Snapshot(context.Background(), f, testOpts())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	d := Adoptable(KindNetwork, "core-top-net", snap, nsNames)
	if !d.Adoptable {
		t.Errorf("labeled network: %+v", d)
	}
	d = Adoptable(KindVolume, "default-website-data", snap, nsNames)
	if d.Adoptable || d.Reason != ReasonRefusedForeignIdentifier {
		t.Errorf("unlabeled volume: %+v", d)
	}
	d = Adoptable(KindVolume, "no-such-volume", snap, nsNames)
	if d.Adoptable || d.Reason != ReasonAbsentSnapshot {
		t.Errorf("absent volume: %+v", d)
	}
}

// TestDeletableRefusals covers every refusal case of the two-key rule
// against a live ledger + fresh snapshot, with typed reasons.
func TestDeletableRefusals(t *testing.T) {
	ctx := context.Background()
	f := fixture(t)
	snap, err := Snapshot(context.Background(), f, testOpts())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// Ledger states via MarkCreated → managed for "default-website"
	// (created by TOP, labeled app-like), plus a pre-existing foreign
	// container adopted only into pre-existing state.
	st := openLedgerStore(t)
	if err := MarkCreated(ctx, st, KindApp, "default-website", "op-install-1", nil, now); err != nil {
		t.Fatalf("MarkCreated: %v", err)
	}
	if err := st.MarkManaged(ctx, KindApp, "default-website"); err != nil {
		t.Fatalf("MarkManaged: %v", err)
	}
	// Ledger knows an app that the fresh snapshot no longer reports at all
	// (deployed by TOP, then the app vanished/rebuilt outside of TOP):
	// absent-from-snapshot, nothing to delete.
	if err := MarkCreated(ctx, st, KindApp, "default-dead-app", "op-install-0", nil, now); err != nil {
		t.Fatalf("MarkCreated (dead app): %v", err)
	}
	if err := st.InsertLedgerEntry(ctx, &store.Resource{
		Kind: KindContainer, Identifier: "redis-cache",
		State: store.ResourcePreExisting, CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed pre-existing: %v", err)
	}

	cases := []struct {
		kind, ident string
		want        bool
		wantWhy     Reason
	}{
		// Managed + fresh label/naming agreement → deletable.
		{KindApp, "default-website", true, ReasonOK},
		// Pre-existing ledger row → hard refusal even with live name match.
		{KindContainer, "redis-cache", false, ReasonRefusedPreExisting},
		// Key 1 fails: nothing in the ledger.
		{KindContainer, "core-top-controller", false, ReasonRefusedLedgerMissing},
		{KindApp, "core-ghost-app", false, ReasonRefusedLedgerMissing},
		// Key 2 fails: foreign name absent the agreement even in the
		// fresh snapshot.
		{KindApp, "redis-cache", false, ReasonRefusedLedgerMissing},
		// Absent from the fresh snapshot: nothing to delete.
		// Ledger knows an app the fresh snapshot no longer reports (deployed
		// by TOP, then rebuilt/deleted outside of TOP): key 1 passes, key 2
		// cannot evaluate → absent-from-snapshot, nothing to delete.
		{KindApp, "default-dead-app", false, ReasonAbsentSnapshot},
	}
	for _, tc := range cases {
		d, err := Deletable(ctx, st, tc.kind, tc.ident, snap, nsNames)
		if err != nil {
			t.Fatalf("Deletable(%s, %s): %v", tc.kind, tc.ident, err)
		}
		if d.Deletable != tc.want || d.Reason != tc.wantWhy {
			t.Errorf("Deletable(%s, %s) = %+v, want deletable=%v reason=%v",
				tc.kind, tc.ident, d, tc.want, tc.wantWhy)
		}
	}
}

// TestDeletableLabelMismatchIsTheSecondKey: the ledger says managed, but
// the FRESH snapshot shows the resource gone TOP-side (relabeled/renamed).
// The name-prefix path also flips when re-created under a foreign name.
func TestDeletableLabelMismatchIsTheSecondKey(t *testing.T) {
	ctx := context.Background()
	st := openLedgerStore(t)
	if err := MarkCreated(ctx, st, KindNetwork, "core-top-net", "op-install-2", nil, now); err != nil {
		t.Fatalf("MarkCreated: %v", err)
	}
	if err := st.MarkManaged(ctx, KindNetwork, "core-top-net"); err != nil {
		t.Fatalf("MarkManaged: %v", err)
	}

	mkSnap := func(networks ...Network) *HostInventory {
		return &HostInventory{Networks: networks}
	}
	// Fresh snapshot still labeled → deletable.
	fresh := mkSnap(Network{Name: "core-top-net", Labels: map[string]string{LabelManaged: ManagedTrue}})
	if d, err := Deletable(ctx, st, KindNetwork, "core-top-net", fresh, nsNames); err != nil || !d.Deletable {
		t.Fatalf("labeled fresh snapshot = d %+v err %v, want deletable", d, err)
	}
	// Label worn off → refusal (diffs from refusal-free truth table).
	unlabeled := mkSnap(Network{Name: "core-top-net"})
	if d, err := Deletable(ctx, st, KindNetwork, "core-top-net", unlabeled, nsNames); err != nil || d.Deletable || d.Reason != ReasonRefusedLabelMismatch {
		t.Fatalf("unlabeled fresh snapshot = d %+v err %v, want %v", d, err, ReasonRefusedLabelMismatch)
	}
}

// TestNameOwnedBy pins the layer-1 naming edge rules directly: prefix
// lookup, prefix-only identifiers rejected, longest/most specific wins is
// not applicable (one pass, first match), empty namespace skipped.
func TestNameOwnedBy(t *testing.T) {
	_, ok := NameOwnedBy("core-", []string{"core"})
	if ok {
		t.Error("prefix-only identifier must not count as owned")
	}
	if ns, ok := NameOwnedBy("core-top-controller", []string{"core"}); !ok || ns != "core" {
		t.Errorf("core-top-controller → (%q, %v), want core/true", ns, ok)
	}
	if _, ok := NameOwnedBy("web", []string{"core", "default"}); ok {
		t.Error("unprefixed identifier must not count as owned")
	}
	_, ok = NameOwnedBy("default-web", []string{"", "default"})
	if !ok {
		t.Error("empty namespace entry must be skipped, not break the match")
	}
}

// TestCreateLedgerFromSnapshotRows: the bootstrap walk records every
// TOP-identifiable resource as pre-existing, reports rows it skipped as
// already recorded, and leaves foreign resources unrecorded.
func TestCreateLedgerFromSnapshotRows(t *testing.T) {
	ctx := context.Background()
	f := fixture(t)
	snap, err := Snapshot(context.Background(), f, testOpts())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	st := openLedgerStore(t)

	// Idempotency: run twice; the second pass skips every row as already
	// recorded. Seed by pre-inserting one expected row to exercise that
	// skip path inside one snapshot.
	if err := st.InsertLedgerEntry(ctx, &store.Resource{
		Kind: KindApp, Identifier: "default-website",
		State: store.ResourcePreExisting, CreatedAt: now,
	}); err != nil {
		t.Fatalf("pre-insert: %v", err)
	}

	report, inserted, err := CreateLedgerFromSnapshot(ctx, st, snap, nsNames, "op-bootstrap-1", now)
	if err != nil {
		t.Fatalf("CreateLedgerFromSnapshot: %v", err)
	}

	// Expected TOP-identifiable set from the fixtures: the two apps by
	// naming; core-top-controller container by managed label;
	// core-legacy-worker + default-website containers by core-/default-
	// naming; core-top-net + observability-web-net networks by label;
	// core-top-state volume by label. redis-cache and the bridge/host
	// networks stay out.
	want := map[string]bool{
		KindApp + "/default-website":           false, // seeded → skipped
		KindApp + "/core-top-controller":       true,
		KindContainer + "/core-top-controller": true,
		KindContainer + "/core-legacy-worker":  true,
		KindContainer + "/default-website":     true,
		KindNetwork + "/core-top-net":          true,
		KindNetwork + "/observability-web-net": true,
		KindVolume + "/core-top-state":         true,
	}
	gotKindIdent := map[string]bool{}
	for _, e := range report {
		key := e.Kind + "/" + e.Identifier
		gotKindIdent[key] = e.Recorded
	}
	for key, recorded := range want {
		got, ok := gotKindIdent[key]
		if !ok {
			t.Errorf("missing ledger row for %s", key)
			continue
		}
		if got != recorded {
			t.Errorf("%s: recorded=%v, want %v", key, got, recorded)
		}
		delete(gotKindIdent, key)
	}
	for key := range gotKindIdent {
		t.Errorf("unexpected ledger row %s (foreign resource recorded?)", key)
	}

	rows, err := st.ListLedger(ctx, "")
	if err != nil {
		t.Fatalf("ListLedger: %v", err)
	}
	for _, r := range rows {
		if r.State != store.ResourcePreExisting {
			t.Errorf("row %s/%s state = %s, want pre-existing", r.Kind, r.Identifier, r.State)
		}
		switch r.Kind {
		case KindApp, KindContainer, KindNetwork, KindVolume:
		default:
			t.Errorf("bad kind %q row", r.Kind)
		}
	}
	if wantRows := 8; len(rows) != wantRows {
		t.Errorf("ledger row count = %d, want %d", len(rows), wantRows)
	}
	if inserted < 6 || inserted > len(report) {
		t.Errorf("inserted=%d, want 6 TOP-identifiable fresh rows of %d report entries", inserted, len(report))
	}
	// Namespace evidence on name-adopted rows without managed labels. The
	// app/dokku row was seeded label-less (the skip path), so the evidence
	// must show on the fresh container row.
	evidence := false
	for _, r := range rows {
		if r.Kind == KindContainer && r.Identifier == "default-website" && r.Labels[LabelNamespace] == "default" {
			evidence = true
		}
	}
	if !evidence {
		t.Error("naming-adopted row must carry top.dev/namespace evidence")
	}
}
