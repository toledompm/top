// SPDX-License-Identifier: MIT

package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/backend/dokku"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/inventory"
	"github.com/toledompm/top/internal/store"
)

// Teardown tests (ADR-0009): two-key refusals never delete, pre-existing
// rows refuse, defaults KEEP data, each tier removes exactly its own
// share, and the ledger-diff report proves what survived.

// testUninstallOptions mirrors testOptions for the teardown surface.
func testUninstallOptions(t *testing.T, r exec.Runner, st store.Store) UninstallOptions {
	t.Helper()
	return UninstallOptions{
		Runner:     r,
		Backend:    dokku.New(r),
		Store:      st,
		DataDir:    filepath.Join(t.TempDir(), "top"),
		Confirm:    true,
		Namespaces: []string{"core", "observability"},
		Now:        func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
	}
}

func seedUninstallDoctor(f *exectest.FakeRunner) {
	f.SetPrefix([]string{"dokku", "version"}, respOK(dkVerOut))
	f.SetPrefix([]string{"dokku", "apps:list"}, respOK("[]"))
	f.SetPrefix([]string{"docker", "version"}, respOK(dockerVer))
	f.SetPrefix([]string{"sudo"}, respOK(""))
}

// seedSnapshotApps installs the apps-list JSON for the teardown snapshot.
func seedSnapshotApps(f *exectest.FakeRunner, apps []string) {
	quoted := []string{}
	for _, a := range apps {
		quoted = append(quoted, `"`+a+`"`)
	}
	f.SetPrefix([]string{"dokku", "apps:list"}, respOK("["+strings.Join(quoted, ",")+"]"))
}

// seedOwnHostLedger writes the ledger rows the M2 install would have produced.
func seedOwnHostLedger(t *testing.T, st store.Store, rows []*store.Resource) {
	t.Helper()
	now := time.Now().UTC()
	for _, row := range rows {
		if row.CreatedAt.IsZero() {
			row.CreatedAt = now
		}
		if err := st.InsertLedgerEntry(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
}

// seedInstalledFS builds the fixture data dir: secrets, git keys, top.db
// placeholder files, backups, plus an inventory cmd json file.
func seedInstalledFS(t *testing.T, dataDir string) string {
	t.Helper()
	for _, sub := range []string{"", "secrets", "git", "backups"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fd1 := filepath.Join(dataDir, "secrets", "provider-token")
	if err := os.WriteFile(fd1, []byte("TKN-FAKE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretName := "provider-token"
	if secretName == "" {
		t.Fatal("fixture secret name")
	}
	for _, name := range []string{"deploy_key", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(dataDir, "git", name), []byte("FAKEKEY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"top.db", "inventory-op-fixture.json"} {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte("fixture data, not a real sqlite file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, "backups", "backup-fixture.sql"), []byte("-- fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return secretName
}

// TestUninstallNothingInstalled: a missing top.db is a refused,
// instructed clean stop — never a fresh empty ledger to work through.
func TestUninstallNothingInstalled(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	opts := testUninstallOptions(t, f, nil) // Store nil → opens dataDir/top.db

	res, err := Uninstall(context.Background(), opts)
	if !errors.Is(err, ErrNothingInstalled) {
		t.Fatalf("err = %v, want ErrNothingInstalled", err)
	}
	if got := recordedArgvOf(f); len(got) != 0 {
		t.Fatalf("nothing-installed run issued argv: %v", got)
	}
	if res != nil && len(res.Removed) != 0 {
		t.Fatalf("nothing may be removed without ownership proof: %+v", res.Removed)
	}
}

func TestUninstallGateRefused(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)
	opts.Confirm = false

	seedUninstallDoctor(f)
	seedSnapshot(f)

	if _, err := Uninstall(context.Background(), opts); !errors.Is(err, ErrGateRequired) {
		t.Fatalf("err = %v, want ErrGateRequired", err)
	}
	if got := recordedArgvOf(f); len(got) != 0 {
		t.Fatalf("refused run issued argv: %v", got)
	}
}

// TestUninstallDryRunPlan: the decision list with zero mutations — no
// destroy argv, no store writes (no uninstall op, no ledger drops).
func TestUninstallDryRunPlan(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)
	opts.DryRun = true

	seedUninstallDoctor(f)
	seedSnapshotApps(f, []string{"core-top-controller", "foreign-app"})
	seedSnapshot(f)
	seedOwnHostLedger(t, st, []*store.Resource{{
		Kind:        inventory.KindApp,
		Identifier:  "core-top-controller",
		Labels:      map[string]string{inventory.LabelManaged: inventory.ManagedTrue, inventory.LabelNamespace: "core", inventory.LabelService: "top-controller"},
		CreatedByOp: "fixture-op-install",
		State:       store.ResourceBootstrapOwned,
	}})

	res, err := Uninstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("uninstall dry-run: %v", err)
	}
	if !slices.ContainsFunc(res.Removed, func(s string) bool {
		return strings.Contains(s, "app core-top-controller")
	}) {
		t.Errorf("dry-run plan must list the would-remove items: %+v", res.Removed)
	}
	got := recordedArgvOf(f)
	for _, line := range got {
		if strings.Contains(line, "apps:destroy") {
			t.Fatalf("dry-run issued a destroy: %s", line)
		}
	}
	var uninstallOps int
	ops, _ := st.ListOperations(context.Background(), 100, &store.OperationFilter{Kind: domain.OpUninstall})
	uninstallOps = len(ops)
	if uninstallOps != 0 {
		t.Errorf("dry-run recorded operations: %d", uninstallOps)
	}
	if rows := ledgerRows(t, st, ""); len(rows) != 1 {
		t.Errorf("dry-run must leave the ledger intact: %+v", rows)
	}
}

// TestUninstallRemovesOwnedKeepsForeign — the acceptance posture: full
// teardown removes exactly the ledgered TOP app; foreign state never gets
// a verb.
func TestUninstallRemovesOwnedKeepsForeign(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)

	seedUninstallDoctor(f)
	seedSnapshotApps(f, []string{"core-top-controller", "foreign-app"})
	seedSnapshot(f)
	seedOwnHostLedger(t, st, []*store.Resource{{
		Kind:        inventory.KindApp,
		Identifier:  "core-top-controller",
		Labels:      map[string]string{inventory.LabelManaged: inventory.ManagedTrue, inventory.LabelNamespace: "core", inventory.LabelService: "top-controller"},
		CreatedByOp: "fixture-op-install",
		State:       store.ResourceBootstrapOwned,
	}})
	f.SetPrefix([]string{"dokku", "apps:exists"}, respOK("App exists"))
	seedTeardown(f)

	res, err := Uninstall(ctx, opts)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got := recordedArgvOf(f)
	if !slices.Contains(got, "dokku apps:destroy --force core-top-controller") {
		t.Errorf("controller app must be destroyed; argv:\n%s", strings.Join(got, "\n"))
	}
	for _, line := range got {
		if strings.Contains(line, "foreign-app") {
			t.Fatalf("a foreign app was given a verb: %s", line)
		}
	}
	if rows := ledgerRows(t, st, ""); len(rows) != 0 {
		t.Errorf("removed app's ledger row must drop after success: %+v", rows)
	}
	if !slices.ContainsFunc(res.Removed, func(s string) bool {
		return strings.HasPrefix(s, "app core-top-controller (was bootstrap-owned")
	}) {
		t.Errorf("remove list must record the removed app + provenance: %+v", res.Removed)
	}
	if opState := newestOpState(t, st, domain.OpUninstall); opState != domain.OpSucceeded {
		t.Errorf("uninstall op state = %v, want succeeded", opState)
	}
}

func newestOpState(t *testing.T, st store.Store, kind domain.OperationKind) domain.OperationState {
	t.Helper()
	ops, err := st.ListOperations(context.Background(), 1, &store.OperationFilter{Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) == 0 {
		return ""
	}
	return ops[0].State
}

// TestUninstallTwoKeyRefusals: both keys must agree — a pre-existing row
// refuses on key 1; a ledger row whose resource vanished from the fresh
// snapshot refuses on key 2. Nobody deletes anything but the healthy pair.
func TestUninstallTwoKeyRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)

	seedUninstallDoctor(f)
	seedSnapshotApps(f, []string{"core-top-controller", "core-prometheus"})
	seedSnapshot(f)
	seedOwnHostLedger(t, st, []*store.Resource{
		{
			Kind:        inventory.KindApp,
			Identifier:  "core-top-controller",
			Labels:      map[string]string{inventory.LabelManaged: inventory.ManagedTrue, inventory.LabelNamespace: "core", inventory.LabelService: "top-controller"},
			CreatedByOp: "fixture-op-install",
			State:       store.ResourceBootstrapOwned,
		},
		{
			// Pre-existing: adoption never happened; deletion refused.
			Kind:        inventory.KindApp,
			Identifier:  "core-prometheus",
			Labels:      map[string]string{inventory.LabelNamespace: "core"},
			CreatedByOp: "fixture-op-inventory",
			State:       store.ResourcePreExisting,
		},
		{
			// Ledger knows it, snapshot doesn't: key 2 fails.
			Kind:        inventory.KindApp,
			Identifier:  "core-ghost-app",
			Labels:      map[string]string{inventory.LabelManaged: inventory.ManagedTrue, inventory.LabelNamespace: "core"},
			CreatedByOp: "fixture-op-install",
			State:       store.ResourceBootstrapOwned,
		},
	})
	f.SetPrefix([]string{"dokku", "apps:exists"}, respOK("App exists"))
	seedTeardown(f)

	res, err := Uninstall(ctx, opts)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	got := recordedArgvOf(f)
	if !slices.Contains(got, "dokku apps:destroy --force core-top-controller") {
		t.Errorf("the healthy pair must still be removable; argv:\n%s", strings.Join(got, "\n"))
	}
	for _, refused := range []string{"core-prometheus", "core-ghost-app"} {
		if slices.ContainsFunc(got, func(line string) bool {
			return strings.Contains(line, refused)
		}) {
			t.Fatalf("a two-key-refused app got a verb: %s", refused)
		}
		if !slices.ContainsFunc(res.Skipped, func(s string) bool {
			return strings.Contains(s, refused)
		}) {
			t.Errorf("refusal must be reported with reason for %s: %+v", refused, res.Skipped)
		}
	}
	// The refused rows stay ledgered (rows remain for re-runs).
	remaining := ledgerRows(t, st, "")
	for _, refused := range []string{"core-prometheus", "core-ghost-app"} {
		if !slices.ContainsFunc(remaining, func(r *store.Resource) bool {
			return r.Identifier == refused
		}) {
			t.Errorf("refused row must stay for the re-run: %s", refused)
		}
	}
}

// TestUninstallDestroyOrderReverseNamespace: apps die default →
// observability → core (the controller last).
func TestUninstallDestroyOrderReverseNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)
	opts.Namespaces = []string{"core", "observability", "default"}

	seedUninstallDoctor(f)
	seedSnapshotApps(f, []string{"default-web", "observability-prometheus", "core-top-controller"})
	seedSnapshot(f)
	const manged = inventory.ManagedTrue
	rows := []*store.Resource{}
	for _, app := range []string{"default-web", "observability-prometheus", "core-top-controller"} {
		ns, _, _ := strings.Cut(app, "-")
		rows = append(rows, &store.Resource{
			Kind:        inventory.KindApp,
			Identifier:  app,
			Labels:      map[string]string{inventory.LabelManaged: manged, inventory.LabelNamespace: ns},
			CreatedByOp: "fixture-op-install",
			State:       store.ResourceBootstrapOwned,
		})
	}
	seedOwnHostLedger(t, st, rows)
	f.SetPrefix([]string{"dokku", "apps:exists"}, respOK("App exists"))
	seedTeardown(f)

	if _, err := Uninstall(ctx, opts); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	var destroyOrder []string
	for _, c := range f.Recorded() {
		if c.Name == "dokku" && slices.Contains(c.Args, "apps:destroy") {
			destroyOrder = append(destroyOrder, c.Args[len(c.Args)-1])
		}
	}
	want := []string{"default-web", "observability-prometheus", "core-top-controller"}
	if !slices.Equal(destroyOrder, want) {
		t.Errorf("destroy order = %v, want %v (controller last)", destroyOrder, want)
	}
}

// TestUninstallPurgeTiers: defaults keep every byte of state; --purge-data
// removes the SQLite state + backups + storage entry (NOT secrets/git);
// --purge-secrets removes the secret files + git key material.
func TestUninstallPurgeTiers(t *testing.T) {
	t.Parallel()

	t.Run("default-keeps-everything", func(t *testing.T) {
		f := exectest.NewFakeRunner()
		st := testStore(t)
		opts := testUninstallOptions(t, f, st)
		secretName := seedInstalledFS(t, opts.DataDir)
		seedUninstallDoctor(f)
		seedSnapshotApps(f, []string{})
		seedSnapshot(f)
		seedOwnHostLedger(t, st, nil)

		_ = secretName
		if _, err := Uninstall(context.Background(), opts); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
		for _, kept := range []string{"top.db", "provider-token", "deploy_key", "known_hosts", "backup-fixture.sql"} {
			found := false
			for _, walk := range []string{"", "secrets", "git", "backups"} {
				if _, err := os.Stat(filepath.Join(opts.DataDir, walk, kept)); err == nil {
					found = true
				}
			}
			if !found {
				t.Errorf("%s must survive the default teardown", kept)
			}
		}
		if len(res0(t, st)) != 0 {
			t.Errorf("nothing to remove — no destroy argv expected")
		}
	})

	t.Run("purge-data", func(t *testing.T) {
		f := exectest.NewFakeRunner()
		st := testStore(t)
		opts := testUninstallOptions(t, f, st)
		seedInstalledFS(t, opts.DataDir)
		seedUninstallDoctor(f)
		seedSnapshotApps(f, []string{"core-top-controller"})
		seedSnapshot(f)
		seedOwnHostLedger(t, st, []*store.Resource{{
			Kind:        inventory.KindApp,
			Identifier:  "core-top-controller",
			Labels:      map[string]string{inventory.LabelManaged: inventory.ManagedTrue, inventory.LabelNamespace: "core", inventory.LabelService: "top-controller"},
			CreatedByOp: "fixture-op-install",
			State:       store.ResourceBootstrapOwned,
		}})
		f.SetPrefix([]string{"dokku", "apps:exists"}, respOK("App exists"))
		seedTeardown(f)
		opts.PurgeData = true

		res, err := Uninstall(context.Background(), opts)
		if err != nil {
			t.Fatalf("uninstall --purge-data: %v", err)
		}
		got := recordedArgvOf(f)
		if !slices.Contains(got, "dokku storage:destroy top-metadata --force") {
			t.Errorf("purge-data must destroy the storage entry; argv:\n%s", strings.Join(got, "\n"))
		}
		if !slices.Contains(got, "dokku storage:unmount core-top-controller top-metadata --container-dir "+opts.DataDir) {
			t.Errorf("purge-data must detach the state mount first; argv:\n%s", strings.Join(got, "\n"))
		}
		// SQLite/backups/inventory gone; secrets + git keys remain.
		for _, gone := range []string{"top.db", "inventory-op-fixture.json", "backup-fixture.sql"} {
			if _, err := os.Stat(filepath.Join(opts.DataDir, gone)); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					t.Errorf("%s must be removed by --purge-data", gone)
				}
			}
		}
		for _, kept := range []string{filepath.Join("secrets", "provider-token"), filepath.Join("git", "deploy_key")} {
			if _, err := os.Stat(filepath.Join(opts.DataDir, kept)); err != nil {
				t.Errorf("%s must survive --purge-data (retained unless --purge-secrets): %v", kept, err)
			}
		}
		if !slices.ContainsFunc(res.LeftAlone, func(s string) bool {
			return strings.Contains(s, "purge-secrets to delete")
		}) {
			t.Errorf("kept-secrets note must be in the report: %+v", res.LeftAlone)
		}
	})

	t.Run("purge-data-plus-secrets", func(t *testing.T) {
		f := exectest.NewFakeRunner()
		st := testStore(t)
		opts := testUninstallOptions(t, f, st)
		seedInstalledFS(t, opts.DataDir)
		seedUninstallDoctor(f)
		seedSnapshotApps(f, []string{})
		seedSnapshot(f)
		seedOwnHostLedger(t, st, nil)
		seedTeardown(f)
		opts.PurgeData = true
		opts.PurgeSecrets = true

		res, err := Uninstall(context.Background(), opts)
		if err != nil {
			t.Fatalf("uninstall full purge: %v", err)
		}
		if !slices.ContainsFunc(res.Removed, func(s string) bool {
			return strings.Contains(s, "removed secret secret:provider-token")
		}) {
			t.Errorf("purge-secrets must list each deleted secret (names, never values): %+v", res.Removed)
		}
		if _, err := os.Stat(opts.DataDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the data dir must be gone after both purges: %v", err)
		}
	})
}

func res0(t *testing.T, st store.Store) []string {
	t.Helper()
	rows := ledgerRows(t, st, "")
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Kind+"/"+r.Identifier)
	}
	return out
}

// TestUninstallDokkuPrintsManualRunbook: the tier is approved via its own
// flag, but the M2 slice prints the documented runbook rather than
// auto-running the upstream purge (documented deviation; M3 hardening).
func TestUninstallDokkuPrintsManualRunbook(t *testing.T) {
	t.Parallel()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)
	opts.UninstallDokku = true

	seedUninstallDoctor(f)
	seedSnapshotApps(f, []string{})
	seedSnapshot(f)
	seedOwnHostLedger(t, st, nil)
	seedTeardown(f)

	res, err := Uninstall(context.Background(), opts)
	if err != nil {
		t.Fatalf("uninstall --uninstall-dokku: %v", err)
	}
	if !slices.ContainsFunc(res.Commands, func(c string) bool {
		return strings.Contains(c, "remove dokku")
	}) {
		t.Errorf("dokku purge runbook must be printed: %+v", res.Commands)
	}
	if !slices.ContainsFunc(res.Skipped, func(s string) bool {
		return strings.Contains(s, "NOT uninstalled in the M2 slice")
	}) {
		t.Errorf("the refusal-with-reason must surface: %+v", res.Skipped)
	}
	for _, c := range f.Recorded() {
		if c.Name == "sudo" && (len(c.Args) != 2 || c.Args[0] != "-n" || c.Args[1] != "true") {
			t.Fatalf("M2 slice must not auto-execute the dokku purge: %s", c.String())
		}
	}
}

// TestUninstallLeaseHeld: uninstall refuses while another writer holds
// the controller lease (the "stop accepting triggers" precondition).
func TestUninstallLeaseHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := exectest.NewFakeRunner()
	st := testStore(t)
	opts := testUninstallOptions(t, f, st)

	seedUninstallDoctor(f)
	seedSnapshotApps(f, []string{})
	seedSnapshot(f)
	seedOwnHostLedger(t, st, nil)

	// Another writer holds the lease NOW (unexpired).
	if ok, err := st.AcquireLease(ctx, "controller", "running-controller", 5*time.Minute, false); err != nil || !ok {
		t.Fatalf("fixture lease acquire failed: ok=%t err=%v", ok, err)
	}

	_, err := Uninstall(ctx, opts)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("err = %v, want ErrLeaseHeld", err)
	}
	got := recordedArgvOf(f)
	for _, line := range got {
		if strings.Contains(line, "apps:destroy") {
			t.Fatalf("lease-held run must not destroy anything: %s", line)
		}
	}
}
