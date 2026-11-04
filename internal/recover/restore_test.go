// SPDX-License-Identifier: MIT

package recover

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/exec/exectest"
	"github.com/toledompm/top/internal/store/sqlite"
)

// T-503 restore tests. HERMETIC discipline (this package's rule): the
// fake runner is the ONLY process seam — no test may run a real
// dokku/rsync process. The two safeguard proofs the acceptance names
// headline the file:
//
//   - TestRestoreRefusesOlderWatermark (contract safeguard 2: refusing is
//     a typed error PRE-mutation — the live DB is bit-identical after the
//     refusal, no snapshot, no staging temp, no replace);
//   - TestRestoreSafetySnapshotPrecedesMutation (safeguard 3: when the
//     safety snapshot cannot be created, restore aborts BEFORE the
//     replace — the live DB is bit-identical).
//
// Test-modified seeded facts: like bumpSchemaVersion (the documented
// carve-out in testhelpers_test.go), retimeNewestOp adjusts
// `operations.created_at` directly — InsertOperation stamps time.Now and
// the watermark comparison is TEMPORAL (operation IDs are uuids), so the
// timestamps ARE the value under test.

// healthSetpreps the fake runner for the dokku calls the full flow makes:
// ps:stop + ps:restart green, ps:report healthy.
func healthSet(t *testing.T) *exectest.FakeRunner {
	t.Helper()
	r := exectest.NewFakeRunner()
	r.SetOutput("dokku", exectest.Response{}, "ps:stop", controllerAppFixture)
	r.SetOutput("dokku", exectest.Response{}, "ps:restart", controllerAppFixture)
	r.SetOutput("dokku", exectest.Response{Stdout: []byte(reportJSON(true, true))},
		"ps:report", controllerAppFixture, "--format", "json")
	return r
}

func greenRunner(t *testing.T) *exectest.FakeRunner {
	t.Helper()
	r := exectest.NewFakeRunner()
	r.SetDefault(exectest.Response{})
	return r
}

// retimeNewestOp is TEST-MODIFIED SEEDED FACT support (see file header).
func retimeNewestOp(t *testing.T, path string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE operations SET created_at = ?
		WHERE id = (SELECT id FROM operations ORDER BY created_at DESC, id DESC LIMIT 1)`,
		at.UnixNano()); err != nil {
		t.Fatalf("retime newest op in %s: %v", path, err)
	}
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixtureOpStore creates a real, migrated store with one op in it
// (opAt zero value → InsertOperation's own time.Now stands).
func fixtureOpStore(t *testing.T, path string, opAt time.Time) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	op, oerr := domain.NewOperation(domain.OpReconcile, domain.ActorUser, "restore-fixture", "abc123def456")
	if oerr != nil {
		t.Fatal(oerr)
	}
	if err := st.InsertOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if !opAt.IsZero() {
		retimeNewestOp(t, path, opAt)
	}
}

func leaseHeld(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM leases WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func dirEntriesOf(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// ---------------------------------------------------------------------
// Safeguard proofs (the acceptance headlines).

func TestRestoreRefusesOlderWatermark(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(200, 0)) // live op is NEWEST
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Unix(100, 0)) // backup op is OLDER
	before := readFileBytes(t, live)

	res, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if !errors.Is(err, ErrRestoreWatermark) {
		t.Fatalf("want ErrRestoreWatermark, got %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("refusal must tell the --force path: %v", err)
	}
	// Typed refusal is pre-mutation: the live file is BIT-IDENTICAL.
	if after := readFileBytes(t, live); string(before) != string(after) {
		t.Error("the live database file changed during a refusing restore")
	}
	entries := dirEntriesOf(t, dir)
	for name := range entries {
		if name == "top.db" || name == "backup.db" || name == "top-config.yaml" {
			continue
		}
		if strings.HasPrefix(name, "top-") || strings.Contains(name, "restore") {
			t.Errorf("unexpected artifact %q after a refusing restore", name)
		}
	}
	// Safeguard-1 ran (the contract's order puts the stop first): exactly
	// one dokku command was issued.
	if got := strings.Join(r.RecordedCommands(), "\n"); got != "dokku ps:stop core-top-controller" {
		t.Errorf("command log during refusal:\n%s", got)
	}
	_ = res
}

func TestRestoreSafetySnapshotPrecedesMutation(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Unix(100, 0)) // EQUAL watermark → admitted
	// The safety namespace CANNOT be created: phase 3 fails before any
	// replace happens (bit-identity proof, phase 4).
	if err := os.MkdirAll(filepath.Join(dir, "backups"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "backups"), 0o700) })
	before := readFileBytes(t, live)

	_, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if !errors.Is(err, ErrRestoreSafetySnapshot) {
		t.Fatalf("want ErrRestoreSafetySnapshot, got %v", err)
	}
	if !strings.Contains(err.Error(), "nothing has been replaced") {
		t.Errorf("refusal must say nothing was replaced: %v", err)
	}
	if after := readFileBytes(t, live); string(before) != string(after) {
		t.Error("the live database file changed although the safety snapshot failed")
	}
	if leaseHeld(t, live, RestoreLeaseName) {
		t.Error("the restore lease leaked after a failed safety snapshot")
	}
}

// ---------------------------------------------------------------------
// Happy paths and the rest of the refusal matrix.

func TestRestoreHappyPathEqualWatermarkNoForce(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Unix(100, 0)) // equal → no --force needed

	res, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	want := "dokku ps:stop core-top-controller\ndokku ps:restart core-top-controller\ndokku ps:report core-top-controller --format json"
	if got := strings.Join(r.RecordedCommands(), "\n"); got != want {
		t.Errorf("command log:\n%s\nwant:\n%s", got, want)
	}
	// The safety snapshot was created into the safety namespace.
	safety := dirEntriesOf(t, filepath.Join(dir, "backups", "safety"))
	if len(safety) != 1 {
		t.Errorf("want exactly one safety snapshot, got %v", safety)
	}
	// The restored DB is bit-equal to the backup (atomic replace took the
	// backup's bytes + lease released + hints printed).
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "topctl reconcile --now") {
		t.Errorf("the reconciliation hint is missing:\n%s", joined)
	}
	if !strings.Contains(joined, "safety snapshot") {
		t.Errorf("the safety snapshot evidence is missing:\n%s", joined)
	}
	if leaseHeld(t, live, RestoreLeaseName) {
		t.Error("the restore lease leaked after success")
	}
	// The restored watermark equals the backup's: reopen and read. (The
	// live file IS the fsync'd-renamed backup content, so the seeded
	// created_at rides along.)
	p, perr := probeStoreFile(context.Background(), live)
	if perr != nil {
		t.Fatalf("probe restored db: %v", perr)
	}
	b, _ := probeStoreFile(context.Background(), backup)
	if p.newestOpID != b.newestOpID || p.newestOpAtNanos != b.newestOpAtNanos {
		t.Errorf("restored watermark %s@%d, want the backup's %s@%d",
			p.newestOpID, p.newestOpAtNanos, b.newestOpID, b.newestOpAtNanos)
	}
}

func TestRestoreForceAcceptsRefusedRollback(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(200, 0))
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Unix(100, 0))

	res, err := Restore(context.Background(), freshOpts(t, r, dir), backup, true, false)
	if err != nil {
		t.Fatalf("Restore --force: %v", err)
	}
	p, perr := probeStoreFile(context.Background(), live)
	if perr != nil {
		t.Fatalf("probe restored db: %v", perr)
	}
	b, _ := probeStoreFile(context.Background(), backup)
	if p.newestOpID != b.newestOpID {
		t.Errorf("restored watermark %s, want the backup's %s", p.newestOpID, b.newestOpID)
	}
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "--force") {
		t.Errorf("the force note must be in the runbook output:\n%s", joined)
	}
}

func TestRestoreDryRunRunsNothing(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))
	before := readFileBytes(t, live)

	res, err := Restore(context.Background(), freshOpts(t, r, dir), live, false, true)
	if err != nil {
		t.Fatalf("dry-run Restore: %v", err)
	}
	if calls := r.Recorded(); len(calls) != 0 {
		t.Errorf("dry-run executed %d commands: %v", len(calls), calls)
	}
	if after := readFileBytes(t, live); string(before) != string(after) {
		t.Error("dry-run modified the live database")
	}
	if !strings.Contains(strings.Join(res.Commands, "\n"), "dokku ps:stop") {
		t.Errorf("dry-run must print the plan:\n%s", strings.Join(res.Commands, "\n"))
	}
}

func TestRestoreRefusesCorruptBackup(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))
	backup := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(backup, []byte("this is not a SQLite file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if !errors.Is(err, ErrRestoreCorrupt) {
		t.Fatalf("want ErrRestoreCorrupt, got %v", err)
	}
	if entries := dirEntriesOf(t, filepath.Join(dir, "backups")); len(entries) != 0 {
		t.Errorf("a refusing restore must not snapshot: %v", entries)
	}
}

func TestRestoreRefusesSchemaNewerBackup(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Time{})
	bumpSchemaVersion(t, backup, 99) // newer-build simulation (testhelpers carve-out)

	_, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if !errors.Is(err, ErrRestoreSchemaNewer) {
		t.Fatalf("want ErrRestoreSchemaNewer, got %v", err)
	}
}

func TestRestoreNoLiveDBRefused(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Time{})

	_, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if !errors.Is(err, ErrRestoreNoLiveDB) {
		t.Fatalf("want ErrRestoreNoLiveDB, got %v", err)
	}
}

func TestRestoreInFlightRefused(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Time{})
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Time{})

	// Hold the lease the way a concurrent restore would.
	st, err := sqlite.Open(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	ok, lerr := st.AcquireLease(context.Background(), RestoreLeaseName, fakeHolder, RestoreLeaseTTL, false)
	if lerr != nil || !ok {
		t.Fatal("fixture could not acquire the restore lease")
	}

	_, err = Restore(context.Background(), freshOpts(t, r, dir), backup, false, false)
	if !errors.Is(err, ErrRestoreInFlight) {
		t.Fatalf("want ErrRestoreInFlight, got %v", err)
	}
	_ = st.Close()
}

func TestRestoreRemoteSpecPullsBeforeLocalWork(t *testing.T) {
	t.Parallel()
	r := greenRunner(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))

	// The remote file refuser: rsync fails (destination down). The fake
	// runner returns the default error.
	spec := "user@example-host:/var/backups/top-2026.db"
	_, err := Restore(context.Background(), freshOpts(t, r, dir), spec, false, false)
	if err == nil || !strings.Contains(err.Error(), "offsite pull") {
		t.Fatalf("want the offsite pull failure, got %v", err)
	}
	var calls []string
	for _, c := range r.Recorded() {
		calls = append(calls, c.String())
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "dokku ps:stop core-top-controller") {
		t.Errorf("the stop (contract safeguard 1) must precede the pull:\n%s", joined)
	}
	if !strings.Contains(joined, "rsync -e ssh -o ConnectTimeout=15 -o BatchMode=yes "+spec) {
		t.Errorf("rsync argv missing or notBatchMode'd:\n%s", joined)
	}
	// The remote pull lands in remote-staging — nothing staged here, so
	// nothing replaced the live DB.
	p, perr := probeStoreFile(context.Background(), live)
	if perr != nil {
		t.Fatalf("probe live db: %v", perr)
	}
	if p.newestOpID == "" {
		t.Error("live db lost its operation though restore never admitted")
	}
}

func TestRestoreRemovesStaleSidecars(t *testing.T) {
	t.Parallel()
	r := healthSet(t)
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Unix(100, 0))
	backup := filepath.Join(dir, "backup.db")
	fixtureOpStore(t, backup, time.Unix(100, 0)) // equal watermark → admitted
	// Stale sidecars from the pre-restore DB must not survive the replace
	// (a leftover -wal attached to the new file would resurrect
	// pre-restore pages — replace mechanics proposal note).
	if err := os.WriteFile(live+"-wal", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), freshOpts(t, r, dir), backup, false, false); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(live + "-wal"); err == nil {
		t.Error("the stale -wal sidecar survived the replace")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sidecar: %v", err)
	}
	// No .top-restore-* staging temps remain after a successful replace.
	for name := range dirEntriesOf(t, dir) {
		if strings.HasPrefix(name, ".top-restore-") {
			t.Errorf("staging temp %s leaked", name)
		}
	}
}

// Redeploy × restore interaction (T-503 item 5, ADR-0005 borrowed path).
func TestRedeployRefusesWhileRestoreLeaseHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live := filepath.Join(dir, "top.db")
	fixtureOpStore(t, live, time.Time{})

	st, err := sqlite.Open(context.Background(), live)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if ok, lerr := st.AcquireLease(context.Background(), RestoreLeaseName, fakeHolder, RestoreLeaseTTL, false); lerr != nil || !ok {
		t.Fatal("fixture could not acquire the restore lease")
	}
	r := healthSet(t)
	err = redeployProbesRestore(context.Background(), freshOpts(t, r, dir))
	if !errors.Is(err, ErrRestoreInFlight) {
		t.Fatalf("want ErrRestoreInFlight, got %v", err)
	}

	// Propagate into Redeploy itself: the mutating path refuses.
	if _, derr := Redeploy(context.Background(), freshOpts(t, r, dir), "", false); !errors.Is(derr, ErrRestoreInFlight) {
		t.Fatalf("Redeploy want ErrRestoreInFlight, got %v", derr)
	}
}

func TestRedeployProbeSilentWhenStoreDown(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // no store at all
	r := healthSet(t)
	if err := redeployProbesRestore(context.Background(), freshOpts(t, r, dir)); err != nil {
		t.Fatalf("a broken store must keep redeploy's restore probe silent, got %v", err)
	}
}

// freshOpts builds the test Options (diagnose_test.go's testOptions with
// a tight health budget so the fake ps:report poll stays hermetic).
func freshOpts(t *testing.T, r *exectest.FakeRunner, dir string) Options {
	t.Helper()
	o := testOptions(t, r, dir, 500<<30)
	o.HealthTimeout = 100 * time.Millisecond
	o.PollInterval = time.Millisecond
	return o
}

// fakeHolder is the fixture holder id of a concurrent restore's lease.
const fakeHolder = "fake-restore-holder"
