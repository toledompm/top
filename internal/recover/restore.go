// SPDX-License-Identifier: MIT

package recover

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/toledompm/top/internal/backup"
	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
	"github.com/toledompm/top/internal/store/sqlite"
)

// Restore — `topctl recover restore --backup <file|user@host:path>` (T-503,
// the M5 body of the backup-recovery contract's "Recovery" restore bullet
// and ADR-0007's "Restore safeguards").
//
// The safeguard order is VERBATIM the contract's
// (docs/contracts/backup-recovery.md: "restore --backup <file> — safeguards
// in order:") and the ADR-0007 list:
//
//  1. refuse unless the controller is stopped — the command stops it via
//     Dokku;
//  2. refuse if the live DB's newest operation is newer than the backup's
//     watermark WITHOUT --force (typed refusal, pre-mutation, no file
//     touched — equality is allowed: restoring the very state that is
//     live is not a rollback loss);
//  3. snapshot the current DB aside — the safety snapshot is a real,
//     registered backup (backup.Engine.Create into <backups>/safety/) and
//     MUST succeed before anything replaces the live file;
//  4. replace the live DB atomically (copied to a same-directory temp
//     file, fsync'd, renamed, directory fsync'd, stale -wal/-shm removed)
//     and run migrations forward (sqlite.Open's goose run);
//  5. restart the controller, wait bounded for health, and print the
//     reconciliation hint (`topctl reconcile --now`).
//
// Deliberate v0 proposals (documented in the contract's "Restore mechanics
// (T-503 — PROPOSAL)" section): no operation record is inserted after the
// restore, so the restored DB's watermark equals the backup's exactly;
// admission is by watermark + integrity of the FILE itself, never by
// registry copy-history (the backup README's offsite note: the offsite
// lifeline is restorable without local registry rows).
//
// Independence (ADR-0005): only the Dokku CLI, the filesystem, and SQLite
// files are touched; a dead controller is stopped as a no-op; every
// failure names the exact manual command equivalents ("recovery output
// doubles as a runbook").

// Typed refusal/failure errors. The CLI maps each to its envelope code;
// the refuse-classes never mutate a file.
var (
	// ErrRestoreWatermark — safeguard 2: the backup is OLDER than the live
	// DB (rollback would lose operations). Refusal happens BEFORE any DB
	// mutation.
	ErrRestoreWatermark = errors.New("refusing: the live database's newest operation is NEWER than the backup's watermark — restoring would lose operations; re-run with --force to accept the rollback loss")
	// ErrRestoreSchemaNewer — the backup was written by a NEWER build than
	// this binary understands; forward-only migration can never read it.
	ErrRestoreSchemaNewer = errors.New("refusing: the backup's schema is newer than this build supports (forward-only migrations cannot read it); upgrade topctl and re-run")
	// ErrRestoreCorrupt — the backup file is not a healthy TOP state store.
	ErrRestoreCorrupt = errors.New("refusing: the backup file failed integrity admission (topctl backup verify for the diagnosis detail)")
	// ErrRestoreSafetySnapshot — safeguard 3 failed; restore aborts with
	// the live DB untouched.
	ErrRestoreSafetySnapshot = errors.New("refusing: the safety snapshot of the current database failed (nothing has been replaced; fix the error and re-run)")
	// ErrRestoreNoLiveDB — restore replaces an existing DB; a missing one
	// is an install problem (fresh-host flow: install first, then
	// restore).
	ErrRestoreNoLiveDB = errors.New("refusing: no live state database found (fresh-host restore installs first: `topctl install`, then restore)")
	// ErrRestoreInFlight — the restore serialization lease ("restore") is
	// held by someone else.
	ErrRestoreInFlight = errors.New("refusing: a restore is already in progress (restore lease held); re-run after the holder exits")
)

// Restore serialization constants (the store's lease table — delivered in
// the M1/M2 store prerequisites for exactly this use; the same lease
// discipline as internal/reconcile).
const (
	// RestoreLeaseName is the serialization-lease name; Redeploy probes
	// this name to refuse overlapping work.
	RestoreLeaseName = "restore"
	// RestoreLeaseTTL bounds a crashed holder's residency.
	RestoreLeaseTTL = 30 * time.Minute
)

// Subdirectories under the backups dir (Options.BackupsDir). "safety" is
// the distinct namespace of safeguard-3 pre-restore snapshots, separate
// from retention-managed backups so a prune policy never silently deletes
// a typed pre-restore copy.
const (
	safetyDirName  = "safety"
	offsiteDirName = "remote-staging"
)

// Restore runs the whole safeguarded restore. backupFlag is the
// operator's --backup value: a local path, or "<user>@<host>:<path>"
// (rsync remote syntax — the Offsite.Pull lifeline for a lost local
// backups directory).
func Restore(ctx context.Context, opts Options, backupFlag string, force, dryRun bool) (*Result, error) {
	opts.applyDefaults()
	app := controllerApp()
	res := &Result{
		App:    app,
		Source: "safeguarded restore (ADR-0007 order: stop → watermark → safety snapshot → atomic replace → migrate → restart)",
	}
	fail := func(err error) (*Result, error) { return res, err }

	if backupFlag == "" {
		return fail(fmt.Errorf("--backup <file-or-user@host:path> is required: %w", domain.ErrInvalid))
	}

	res.Commands = append(res.Commands,
		"dokku ps:stop "+app,
		"dokku ps:restart "+app,
		"topctl reconcile --now")
	if isRemoteSpec(backupFlag) {
		res.Notes = append(res.Notes, fmt.Sprintf("the backup is resolved from the offsite spec %s first (rsync staging under %s)", backupFlag, filepath.Join(opts.BackupsDir, offsiteDirName)))
	}
	if dryRun {
		res.Notes = append(res.Notes, "dry-run: the plan above is what a real run would execute; nothing probed, stopped, or replaced")
		return res, nil
	}

	// Safeguard 1 (contract order): stop the controller via Dokku
	// (idempotent; a dead controller stays dead).
	if _, _, serr := opts.Runner.Run(ctx, "dokku", "ps:stop", app); serr != nil {
		return fail(fmt.Errorf("stop %s failed: %w (exact retry: `dokku ps:stop %s`)", app, serr, app))
	}

	// Admission probes — both files, strictly read-only; nothing mutated.
	live, lerr := probeStoreFile(ctx, dbPath(opts))
	if lerr != nil {
		// Distinct only when the live file does not exist at all: restore
		// replaces a live DB (fresh-host flow installs first, then
		// restores).
		if _, oerr := os.Stat(dbPath(opts)); oerr != nil {
			return fail(fmt.Errorf("%s: %w (%v)", dbPath(opts), ErrRestoreNoLiveDB, lerr))
		}
		return fail(fmt.Errorf("live database %s could not be read (read-only admission probe): %w", dbPath(opts), lerr))
	}
	bpath := backupFlag
	if isRemoteSpec(backupFlag) {
		staged, perr := (&backup.Offsite{
			Runner: opts.Runner,
			Log:    opts.logOrDiscard(),
		}).Pull(ctx, backupFlag, filepath.Join(opts.BackupsDir, offsiteDirName))
		if perr != nil {
			return fail(perr)
		}
		bpath = staged
		res.Notes = append(res.Notes, fmt.Sprintf("backup staged from offsite target %s → %s", backupFlag, staged))
	}
	bak, berr := probeStoreFile(ctx, bpath)
	if berr != nil {
		return fail(fmt.Errorf("backup file %s: %w: %v", bpath, ErrRestoreCorrupt, berr))
	}
	if bak.schemaVersion > sqlite.SchemaVersionMax {
		return fail(fmt.Errorf("backup %s: schema version %d > this build's %d: %w",
			bpath, bak.schemaVersion, sqlite.SchemaVersionMax, ErrRestoreSchemaNewer))
	}

	// Safeguard 2: watermark refuse. The contract's exact wording:
	// "refuse if the live DB's watermark is newer than the backup's
	// without --force" — equality is NOT newer. Operation IDs are not
	// ordered, so "newer" is temporal: each file's NEWEST operation by
	// created_at (internal/store/sqlite's ordering clause:
	// ORDER BY created_at DESC, id DESC).
	if bak.newestOpAtNanos < live.newestOpAtNanos && !force {
		return fail(fmt.Errorf("backup watermark %s (newest op %v) is older than the live watermark %s (newest op %v): %w",
			backupWatermark(bak), opTime(bak.newestOpAtNanos),
			backupWatermark(live), opTime(live.newestOpAtNanos),
			ErrRestoreWatermark))
	}
	if bak.newestOpAtNanos < live.newestOpAtNanos && force {
		res.Notes = append(res.Notes, fmt.Sprintf("--force: accepted the managed rollback past the watermark refusal (live %v older → backup %v)",
			opTime(live.newestOpAtNanos), opTime(bak.newestOpAtNanos)))
	}
	res.Notes = append(res.Notes, fmt.Sprintf("watermark admission ok: backup newest op %s (schema %d) vs live newest op %s (equal-or-older is safe)",
		backupWatermark(bak), bak.schemaVersion, backupWatermark(live)))

	// Safeguard 3: the safety snapshot — a real, registered backup into the
	// safety namespace, which MUST succeed before the replace. (The
	// registry row lands in the live DB about to be replaced — after
	// replace the live registry holds the backup's own rows; the safety
	// FILE is the evidence; outlined in the contract proposal note.)
	st, serr := sqlite.Open(ctx, dbPath(opts))
	if serr != nil {
		return fail(fmt.Errorf("guard probe: open live store %s: %w (manual: `topctl recover diagnose`)", dbPath(opts), serr))
	}
	okl, lerr2 := st.AcquireLease(ctx, RestoreLeaseName, "topctl recover restore", RestoreLeaseTTL, false)
	if lerr2 != nil {
		_ = st.Close()
		return fail(fmt.Errorf("restore lease: %w: %v", ErrRestoreInFlight, lerr2))
	}
	if !okl {
		_ = st.Close()
		return fail(fmt.Errorf("restore lease: %w", ErrRestoreInFlight))
	}
	// Release best-effort: the lease is a serialization hedge, not a
	// completeness claim (the probe path mirrors reconcile's discipline).
	defer func() { _ = st.ReleaseLease(ctx, RestoreLeaseName, "topctl recover restore") }()

	engine := &backup.Engine{
		DBPath:   dbPath(opts),
		Dir:      filepath.Join(opts.BackupsDir, safetyDirName),
		Registry: registryFrom(st),
		Now:      opts.Now,
		Log:      opts.logOrDiscard(),
	}
	safety, serr := engine.Create(ctx)
	if serr != nil {
		// Safeguard-2 of the acceptance order: a failed snapshot aborts
		// restore with NO file replaced (typed refusal; the lease releases
		// via the defer above).
		return fail(fmt.Errorf("%w: %v", ErrRestoreSafetySnapshot, serr))
	}
	_ = st.Close()
	res.Notes = append(res.Notes, fmt.Sprintf("safety snapshot created and registered at %s (newest op %s) BEFORE any file was replaced",
		safety.Path, backupWatermarkFor(safety)))

	// Safeguard 4: the replace — stop-elsewhere noted; the live file is
	// replaced durably and atomically; stale sidecars removed (a leftover
	// -wal attached to the new file would resurrect pre-restore pages).
	bp := dbPath(opts)
	for _, side := range []string{bp + "-wal", bp + "-shm"} {
		_ = os.Remove(side)
	}
	if rerr := atomicReplaceFile(bpath, bp); rerr != nil {
		return fail(fmt.Errorf("replace %s: %w — the pre-restore safety snapshot is at %s (manual recovery: `cp %s %s && dokku ps:restart %s`)",
			bp, rerr, safety.Path, safety.Path, bp, app))
	}

	// Forward migration and verified read-back: sqlite.Open migrates
	// forward and refuses schema-newer loudly (ADR-0007 safeguard 4).
	st2, merr := sqlite.Open(ctx, bp)
	if merr != nil {
		return fail(fmt.Errorf("migrate the restored database %s: %v — the safety snapshot from BEFORE the restore is at %s (manual recovery: `cp %s %s && dokku ps:restart %s`)",
			bp, merr, safety.Path, safety.Path, bp, app))
	}
	after, aerr := probeStoreFile(ctx, bp)
	_ = st2.Close()
	if aerr != nil {
		return fail(fmt.Errorf("verify the restored database %s: %w: %v (safety snapshot at %s)", bp, ErrRestoreCorrupt, aerr, safety.Path))
	}
	if bak.newestOpID != "" && after.newestOpID != bak.newestOpID {
		return fail(fmt.Errorf("restored watermark %s does not match the backup's %s (safety snapshot at %s)", after.newestOpID, bak.newestOpID, safety.Path))
	}
	res.Notes = append(res.Notes, fmt.Sprintf("replace done atomically (fsync'd rename over %s); migrations forward completed; restored watermark %s", bp, backupWatermark(after)))

	// Safeguard 5: restart + bounded health wait + the reconciliation hint.
	res.Commands = append(res.Commands, "dokku ps:restart "+app, "dokku ps:report "+app+" --format json")
	if _, _, err := opts.Runner.Run(ctx, "dokku", "ps:restart", app); err != nil {
		return fail(fmt.Errorf("restart %s after restore: %w (exact retry: `dokku ps:restart %s`; manual emergency rollback: `cp %s %s && dokku ps:restart %s`)",
			app, err, app, safety.Path, bp, app))
	}
	if herr := waitHealthy(ctx, opts, app); herr != nil {
		return fail(herr)
	}
	res.Notes = append(res.Notes, "next step: `topctl reconcile --now` (the contract's reconciliation hint)")
	return res, nil
}

func backupWatermark(p fileProbe) string {
	if p.newestOpID == "" {
		return "(no operations)"
	}
	return p.newestOpID
}

// backupWatermarkFor renders a *store.Backup's registered watermark (the
// engine writes "" for a zero-op store).
func backupWatermarkFor(b *store.Backup) string {
	if b.WatermarkOp == "" {
		return "(no operations)"
	}
	return b.WatermarkOp
}

func opTime(nanos int64) string {
	return time.Unix(0, nanos).UTC().String()
}

// isRemoteSpec reports whether the --backup argument is an rsync remote
// (rsync's "<user>@<host>:<path>" grammar). Local paths carry no "@".
func isRemoteSpec(spec string) bool {
	at := strings.IndexByte(spec, '@')
	colon := strings.IndexByte(spec, ':')
	return at > 0 && colon > at && !strings.ContainsAny(spec, " \t\n\r")
}

// fileProbe is one SQLite state-store file's read-only admission data.
type fileProbe struct {
	newestOpID      string
	newestOpAtNanos int64
	schemaVersion   int64
}

// probeStoreFile opens path STRICTLY READ-ONLY (mode=ro — never migrate,
// never repair; internal/backup's verifyDSNParams pattern) and reads:
// header magic, integrity_check, the newest operation by the store's own
// newest-first ordering, and the applied goose schema version. Structural
// failures classify as corruption.
func probeStoreFile(ctx context.Context, path string) (fileProbe, error) {
	var p fileProbe
	buf := make([]byte, len(headerMagicBytes))
	f, err := os.Open(path)
	if err != nil {
		return p, err // os.ErrNotExist escapes for the no-live-DB case
	}
	_, rerr := io.ReadFull(f, buf)
	_ = f.Close()
	if rerr != nil || !bytes.Equal(buf, headerMagicBytes) {
		return p, domain.CorruptError{Detail: "first 16 bytes are not the SQLite header magic"}
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return p, err
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return p, domain.CorruptError{Detail: fmt.Sprintf("open read-only: %v", err)}
	}
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return p, domain.CorruptError{Detail: fmt.Sprintf("integrity_check: %v", err)}
	}
	msg := ""
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			return p, domain.CorruptError{Detail: "integrity_check: scan: " + err.Error()}
		}
		if line != "ok" && msg == "" {
			msg = "integrity_check: " + line
		}
	}
	_ = rows.Close()
	if re := rows.Err(); re != nil && msg == "" {
		msg = "integrity_check: " + re.Error()
	}
	if msg != "" {
		return p, domain.CorruptError{Detail: msg}
	}
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).
		Scan(&p.schemaVersion); err != nil {
		return p, domain.CorruptError{Detail: "not a TOP state store (goose version table missing): " + err.Error()}
	}
	if err := db.QueryRowContext(ctx,
		`SELECT id, created_at FROM operations ORDER BY created_at DESC, id DESC LIMIT 1`).
		Scan(&p.newestOpID, &p.newestOpAtNanos); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, nil // a valid empty store is a valid restore source
		}
		return p, domain.CorruptError{Detail: "not a TOP state store (operations table missing): " + err.Error()}
	}
	return p, nil
}

// atomicReplaceFile replaces dst with src's contents durably and
// atomically: copy into a same-directory temp file, fsync it, rename over
// dst, fsync the directory (the smallest mechanism honoring "replace" +
// the fsync'd-rename requirement; see the contract's PROPOSAL note).
func atomicReplaceFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".top-restore-")
	if err != nil {
		return fmt.Errorf("staging temp file in %s: %w", filepath.Dir(dst), err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, in); err != nil {
		cleanup()
		return fmt.Errorf("copy: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close copy: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod 0600: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		cleanup()
		return fmt.Errorf("rename onto %s: %w", dst, err)
	}
	d, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("fsync dir: %w", err)
	}
	_ = d.Close()
	return nil
}

// Redeploy guard (T-503 item 5): redeploying while a restore holds the
// lease would let a fresh controller process open a DB file that restore
// is about to rename over. Best-effort probe: a broken store stays silent
// (recovery must survive one — nothing verifiably holds the lease there);
// a HELD lease is a hard refusal.
func redeployProbesRestore(ctx context.Context, opts Options) error {
	st, err := sqlite.Open(ctx, dbPath(opts))
	if err != nil {
		return nil
	}
	ok, lerr := st.AcquireLease(ctx, RestoreLeaseName, "redeploy-probe", time.Minute, false)
	if lerr != nil {
		_ = st.Close()
		return fmt.Errorf("redeploy while restore in flight: %w: %v", ErrRestoreInFlight, lerr)
	}
	if !ok {
		_ = st.Close()
		return fmt.Errorf("redeploy while restore in flight: %w — wait for `topctl recover restore` to finish", ErrRestoreInFlight)
	}
	_ = st.ReleaseLease(ctx, RestoreLeaseName, "redeploy-probe")
	_ = st.Close()
	return nil
}

// headerMagicBytes is the first 16 bytes of every SQLite file (the
// structural sanity check reads that far before opening anything heavier;
// value-for-value internal/backup's headerMagic).
var headerMagicBytes = []byte("SQLite format 3\x00")

// registryFrom wraps the live store for backup.Registry satisfaction; the
// compile-proof that sqlite.Store satisfies Registry lives in the test
// file (internal/backup's own tests do the same).
func registryFrom(st store.Store) backup.Registry {
	return st
}
