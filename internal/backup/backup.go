// SPDX-License-Identifier: MIT

// Package backup is TOP's metadata backup engine: the create/verify/prune
// half of the backup-recovery contract (docs/contracts/backup-recovery.md).
//
// Mechanism (ADR-0007 "Backup mechanism"): one consistent online snapshot
// of the live SQLite state store per run, taken with `VACUUM INTO '<dir>/
// top-<ts>.db'` — never a raw file copy (WAL makes cp-of-the-live-file
// unsafe, ADR-0007 "Alternatives considered"). Scope: TOP metadata only —
// the DB itself, which includes the operation history, ledger, checkpoint,
// and the backup registry. The redacted-config / secret-manifest extension
// is ADR-0013 territory and deliberately NOT in v0 (proposal noted in the
// contract amendment within docs/contracts/backup-recovery.md).
//
// Division of labour:
//
//   - The engine owns mechanics, metrics wiring, and registry bookkeeping
//     — NOT consent. Prune is destructive; Plan computes the script, the
//     caller shows it, and confirmation is the caller's contract with its
//     operator (documented for T-503 in the README + contract amendment).
//   - It depends only on the backup-registry slice of the StateStore
//     through the local Registry seam; internal/store satisfies it and the
//     engine never imports the sqlite implementation package.
//   - Scheduling is caller-invoked v0 (documented deviation): the controller
//     has an event trigger + periodic sweep, not a daily-jobs seam. When
//     the jobs seam lands, it calls this engine per docs/operations/
//     backups.md ("scheduled daily").
//
// Boundary (ADR-0013, worth repeating): TOP backs up TOP. Application
// volumes and databases are the operator's.
package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // engine opens SQLite files directly (driver registration side-effect)

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// Registry is exactly the backup-registry + newest-operation slice of the
// StateStore contract the engine records through
// (docs/contracts/state-store.md): InsertBackup registers (duplicate path →
// ErrConflict), ListBackups returns newest first, SetBackupVerified stamps
// verification, RemoveBackup is the prune-side delete (ErrNotFound on
// unknown paths), ListOperations(1) supplies the watermark.
//
// The v0 SQLite store satisfies this; the engine MUST NOT import
// internal/store/sqlite (one implementation is a detail of the contract,
// not a coupling point).
type Registry interface {
	InsertBackup(ctx context.Context, b *store.Backup) error
	ListBackups(ctx context.Context) ([]*store.Backup, error)
	SetBackupVerified(ctx context.Context, path, verifyResult string, at time.Time) error
	RemoveBackup(ctx context.Context, path string) error
	ListOperations(ctx context.Context, limit int, filter *store.OperationFilter) ([]*domain.Operation, error)
}

// Recorder is the backup-metrics seam. Satisfied by *obs.Metrics
// (SetBackupSuccessAt / SetBackupVerifyResult / RecordBackup); nil-tolerant
// — a backup must succeed where metrics cannot, never the reverse.
type Recorder interface {
	SetBackupSuccessAt(t time.Time)
	SetBackupVerifyResult(ok bool)
	RecordBackup(d time.Duration, result string)
}

// ResultOK / ResultError are the bounded `result` label values this package
// feeds Recorder (obs's set spelled locally so backup never imports obs).
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// verifyResultOK is the registry value of a green verification.
const verifyResultOK = "ok"

// verifyResultLimit caps the recorded failure string (integrity_check can
// emit per-error rows; registry fields stay bounded, per the state-store
// contract's redaction discipline).
const verifyResultLimit = 256

// headerMagic is the first 16 bytes of every SQLite file; the structural
// sanity check reads that far before opening anything heavier.
var headerMagic = []byte("SQLite format 3\x00")

// dsnParams is the ADR-0007 pragma policy for the LIVE-database side of
// Create. Duplicated here (not imported from internal/store/sqlite) so the
// engine never couples to the implementation package; value-for-value it is
// the documented ADR-0007 DSN.
const dsnParams = "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&" +
	"_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

// verifyDSNParams opens a backup file read-only for verification: explicit
// mode=ro so Verify can never migrate or "repair" the file it audits.
const verifyDSNParams = "mode=ro&_pragma=busy_timeout(5000)"

// Engine creates, verifies, and prunes TOP metadata backups within one
// backup directory, against one live state-store database.
type Engine struct {
	// DBPath is the live TOP SQLite state-store file (e.g.
	// /var/lib/top/top.db). It MUST be the same file the Registry reads /
	// writes: registry rows and backup contents are one database, and the
	// watermark is meaningless otherwise.
	DBPath string
	// Dir is the backup destination (ADR-0007: /var/lib/top/backups/,
	// OUTSIDE TOP's app dirs). Created on demand, mode 0o750.
	Dir string
	// Registry is where outcomes are recorded (BackupProvider contract);
	// required.
	Registry Registry
	// Recorder receives backup metrics; optional.
	Recorder Recorder
	// Log receives one info/warn line per operation; optional (discarded
	// when nil).
	Log *slog.Logger
	// Now is the clock (injected for hermetic tests; defaults to time.Now).
	Now func() time.Time
}

// validate normalizes optional fields and rejects broken wiring.
func (e *Engine) validate() error {
	if e.DBPath == "" {
		return fmt.Errorf("backup engine: %w: empty DBPath", domain.ErrInvalid)
	}
	if e.Dir == "" {
		return fmt.Errorf("backup engine: %w: empty Dir", domain.ErrInvalid)
	}
	if e.Registry == nil {
		return fmt.Errorf("backup engine: %w: nil Registry", domain.ErrInvalid)
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Log == nil {
		e.Log = slog.New(slog.DiscardHandler)
	}
	return nil
}

// clock is the normalized UTC now; call validate() first.
func (e *Engine) clock() time.Time { return e.Now().UTC() }

// Create takes one consistent online backup of the live state store and
// registers it (BackupProvider.Create, backup-recovery contract):
//
//  1. watermark = the newest operation ID in the store (the registry row we
//     write after the snapshot is the only intentional post-snapshot
//     mutation, so the watermark is fully contained in the copy),
//  2. `VACUUM INTO <Dir>/top-<ts>.db` from a dedicated connection carrying
//     the ADR-0007 pragma set (never cp of the live file — WAL, ADR-0007),
//  3. fsync of file and directory (the contract's "fsyncs"),
//  4. InsertBackup(size, watermark) into the live registry, then
//     Recorder: SetBackupSuccessAt + RecordBackup(duration, "ok").
//
// A same-second filename collision resolves with a numeric suffix
// (top-…Z-1.db): a re-run must never overwrite a real backup.
func (e *Engine) Create(ctx context.Context) (*store.Backup, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	start := e.Now()
	fail := func(err error) (*store.Backup, error) {
		e.record(start, e.clock(), ResultError)
		e.Log.Warn("backup create failed", "db", e.DBPath, "err", err)
		return nil, fmt.Errorf("backup create: %w", err)
	}

	if err := os.MkdirAll(e.Dir, 0o750); err != nil {
		return fail(fmt.Errorf("prepare backup dir %s: %w", e.Dir, err))
	}

	watermark, err := e.newestOperationID(ctx)
	if err != nil {
		return fail(err)
	}

	target, err := e.nextTargetFile()
	if err != nil {
		return fail(err)
	}

	last, vacuumErr := e.vacuumInto(ctx, target)
	if vacuumErr != nil {
		return fail(vacuumErr)
	}
	size, err := syncFileAndDir(last)
	if err != nil {
		return fail(err)
	}

	b := &store.Backup{
		Path:        last,
		Size:        size,
		WatermarkOp: watermark,
		CreatedAt:   e.clock(),
	}
	if err := e.Registry.InsertBackup(ctx, b); err != nil {
		return fail(err)
	}
	if e.Recorder != nil {
		e.Recorder.SetBackupSuccessAt(e.clock())
	}
	e.record(start, e.clock(), ResultOK)
	e.Log.Info("backup created", "path", last, "size", size, "watermark", watermark)
	return b, nil
}

// Verify checks one backup file (BackupProvider.Verify; ADR-0007
// "Verification & retention"): structural sanity (SQLite header magic —
// read, not opened, first), `PRAGMA integrity_check` over an explicitly
// READ-ONLY connection, and the registered watermark's presence in the
// copy's operation history.
//
// Returns the recorded verification string ("ok" or "failed: <detail under
// the registry limit>"). Whatever the outcome, the registry is stamped via
// SetBackupVerified and the Recorder receives SetBackupVerifyResult. A
// corrupt file classifies as domain.CorruptError; an unregistered path as
// domain.ErrNotFound (the registry is the source of truth — a file nobody
// recorded commands no restore trust).
func (e *Engine) Verify(ctx context.Context, path string) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	row, err := e.registrationFor(ctx, path)
	if err != nil {
		return "", fmt.Errorf("backup verify %s: %w", path, err)
	}

	result := verifyResultOK
	verr := e.verifyFile(ctx, path, row.WatermarkOp)
	if verr != nil {
		result = "failed: " + truncate(verr.Error(), verifyResultLimit)
	}
	if rerr := e.Registry.SetBackupVerified(ctx, path, result, e.clock()); rerr != nil {
		if verr != nil {
			// Keep the diagnosis as the primary error; name the record failure.
			return result, fmt.Errorf("backup verify %s: record failed (%w): %w", path, rerr, verr)
		}
		return result, fmt.Errorf("backup verify %s: %w", path, rerr)
	}
	if e.Recorder != nil {
		e.Recorder.SetBackupVerifyResult(verr == nil)
	}
	if verr != nil {
		e.Log.Warn("backup verify failed", "path", path, "result", result)
		return result, fmt.Errorf("backup verify %s: %w", path, verr)
	}
	e.Log.Info("backup verified", "path", path, "watermark", row.WatermarkOp)
	return result, nil
}

// Plan computes what Prune would do and changes NOTHING — it is the
// caller-consent seam (see prune.go for the retention policy).
func (e *Engine) Plan(ctx context.Context, p Policy) (*Plan, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("backup plan: %w", err)
	}
	return e.plan(ctx, p)
}

// Prune executes retention DESTRUCTIVELY: deletes the pruned files (a
// missing file counts as already deleted; the row-nobody-file lie is what
// Prune can fix) and then their registry rows — file first, row second.
// The first hard error aborts, leaving earlier deletions as performed;
// re-running with the same policy is idempotent.
//
// Consent is the caller's (Plan → show → confirm → Prune); the engine
// refuses to invent an interactive prompt.
func (e *Engine) Prune(ctx context.Context, p Policy) (*Plan, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("backup prune: %w", err)
	}
	plan, err := e.plan(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, b := range plan.Pruned {
		err := os.Remove(b.Path)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			e.Log.Warn("prune: file already missing", "path", b.Path)
		default:
			return plan, fmt.Errorf("backup prune %s: %w", b.Path, err)
		}
		if err := e.Registry.RemoveBackup(ctx, b.Path); err != nil {
			return plan, fmt.Errorf("backup prune %s (registry): %w", b.Path, err)
		}
		e.Log.Info("pruned backup", "path", b.Path, "createdAt", b.CreatedAt)
	}
	return plan, nil
}

// record funnels Create's outcome into the optional metrics recorder.
func (e *Engine) record(start, end time.Time, result string) {
	if e.Recorder == nil {
		return
	}
	e.Recorder.RecordBackup(end.Sub(start), result)
}

// --- internals -------------------------------------------------------------

// newestOperationID returns the newest operation's ID — the backup
// watermark ("" on an empty history; a backup of a valid empty store is a
// valid backup).
func (e *Engine) newestOperationID(ctx context.Context) (string, error) {
	ops, err := e.Registry.ListOperations(ctx, 1, nil)
	if err != nil {
		return "", fmt.Errorf("watermark: %w", err)
	}
	if len(ops) == 0 {
		return "", nil
	}
	return ops[0].ID.String(), nil
}

// nextTargetFile picks Dir/top-<UTC-timestamp>.db with numeric suffixes for
// same-second collisions: the backup directory is append-only, a re-run
// never overwrites a real backup.
func (e *Engine) nextTargetFile() (string, error) {
	base := filepath.Join(e.Dir, fmt.Sprintf("top-%s", e.clock().Format("20060102T150405Z")))
	candidate := base + ".db"
	for i := 1; i < 100; i++ {
		_, err := os.Stat(candidate)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return candidate, nil
		case err != nil:
			return "", fmt.Errorf("backup dir %s: stat target: %w", e.Dir, err)
		}
		candidate = fmt.Sprintf("%s-%d.db", base, i)
	}
	return "", fmt.Errorf("backup dir %s: 99 same-second creates, refusing to guess: %w", e.Dir, domain.ErrConflict)
}

// vacuumInto opens the live DB (dedicated connection, ADR-0007 pragmas) and
// runs the ADR-0007 mechanism: `VACUUM INTO ?`.
//
// Structural sanity on the LIVE side: the DB must actually BE a TOP state
// store (goose version table present). sqlite.Create-file semantics would
// otherwise let this function happily "back up" a nonexistent path by
// creating an empty database — never an acceptable backup.
func (e *Engine) vacuumInto(ctx context.Context, target string) (string, error) {
	if _, err := os.Stat(e.DBPath); os.IsNotExist(err) {
		return "", fmt.Errorf("live database %s: %w", e.DBPath, domain.NewNotFound("state store", e.DBPath))
	}
	db, err := sql.Open("sqlite", "file:"+e.DBPath+"?"+dsnParams)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", e.DBPath, err)
	}
	db.SetMaxOpenConns(1) // the store's documented one-connection posture
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return "", fmt.Errorf("open %s (ping): %w", e.DBPath, err)
	}
	var versions int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM goose_db_version`).Scan(&versions); err != nil {
		return "", fmt.Errorf("live database %s is not a migrated TOP state store (goose version table missing: %v): %w",
			e.DBPath, err, domain.ErrCorrupt)
	}
	if versions == 0 {
		return "", fmt.Errorf("live database %s has no migrations applied (no rows in goose_db_version): %w",
			e.DBPath, domain.ErrCorrupt)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", target, err)
	}
	return target, nil
}

// registrationFor fetches a backup's registry row; unregistered paths are
// refused before anything else (an unchecked file must never look verified).
func (e *Engine) registrationFor(ctx context.Context, path string) (*store.Backup, error) {
	rows, err := e.Registry.ListBackups(ctx)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}
	for _, b := range rows {
		if b.Path == path {
			return b, nil
		}
	}
	return nil, fmt.Errorf("backup %s: %w", path, domain.NewNotFound("backup", path))
}

// verifyFile performs the file-level checks; nil result = trusted.
func (e *Engine) verifyFile(ctx context.Context, path, watermark string) error {
	// Structural sanity 1: it IS (still) a SQLite file.
	buf := make([]byte, len(headerMagic))
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	_, rerr := io.ReadFull(f, buf)
	cerr := f.Close()
	if rerr != nil || cerr != nil {
		return domain.CorruptError{Detail: fmt.Sprintf("read header (read %v, close %v)", rerr, cerr)}
	}
	if !bytes.Equal(buf, headerMagic) {
		return domain.CorruptError{Detail: "first 16 bytes are not the SQLite header magic"}
	}

	// Structural sanity 2 + contract mechanism: READ-ONLY open + integrity
	// check (every emitted row must be "ok"; integrity_check reports one
	// row per defect when the header is broken *inside* valid magic).
	db, err := sql.Open("sqlite", "file:"+path+"?"+verifyDSNParams)
	if err != nil {
		return fmt.Errorf("open verify: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return domain.CorruptError{Detail: fmt.Sprintf("open read-only: %v", err)}
	}
	var msg string
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return domain.CorruptError{Detail: fmt.Sprintf("integrity_check: %v", err)}
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			_ = rows.Close()
			return domain.CorruptError{Detail: "integrity_check: scan: " + err.Error()}
		}
		if line != "ok" && msg == "" {
			msg = "integrity_check: " + line
		}
	}
	if rerr := rows.Err(); rerr != nil && msg == "" {
		msg = "integrity_check: " + rerr.Error()
	}
	_ = rows.Close()
	if msg != "" {
		return domain.CorruptError{Detail: msg}
	}

	// Contract requirement "confirm watermark present": the copy's
	// operation history must contain the registered watermark operation.
	if watermark != "" {
		var n int
		if qerr := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM operations WHERE id = ?`, watermark).Scan(&n); qerr != nil {
			return domain.CorruptError{Detail: "watermark lookup: " + qerr.Error()}
		}
		if n != 1 {
			return domain.CorruptError{
				Detail: fmt.Sprintf("registered watermark %s missing from the backup's operation history", watermark),
			}
		}
	}
	return nil
}

// truncate bounds a detail string for the registry.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// syncFileAndDir fsyncs the new backup file and its directory, then stats
// the size (the contract's "fsyncs"; a directory entry is only as durable
// as its directory).
func syncFileAndDir(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("fsync %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("fsync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("fsync %s (close): %w", path, err)
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return 0, fmt.Errorf("fsync %s (dir): %w", path, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return 0, fmt.Errorf("fsync %s (dir): %w", path, err)
	}
	if err := d.Close(); err != nil {
		return 0, fmt.Errorf("fsync %s (dir close): %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return info.Size(), nil
}
