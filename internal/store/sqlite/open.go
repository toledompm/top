package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pressly/goose/v3"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// SchemaVersionMax is the highest goose schema version this build knows how
// to migrate to. It MUST be bumped with every new migrations/NNNN_*.sql.
const SchemaVersionMax = 1

// dsnParams are the per-connection pragmas from ADR-0007. modernc.org/sqlite
// applies them to every connection opened from this DSN, so pooled and
// transaction connections inherit them:
//
//	journal_mode=WAL   — concurrent readers, crash-safe writes
//	synchronous=NORMAL — safe in WAL; reconcile re-drives the worst case
//	foreign_keys=1     — ledger/run referential integrity is real
//	busy_timeout=5000  — transient cross-writer contention self-heals
const dsnParams = "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&" +
	"_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

// Open opens (and migrates, forward-only) the state store at path; the
// `file:` DSN prefix is added here. `:memory:` is accepted for tests.
//
// Connection policy (ADR-0007, documented tradeoff): a single *sql.DB with
// MaxOpenConns(1) serializes ALL access through exactly one write-capable
// connection. In-process writer contention therefore never reaches SQLite at
// all (no SQLITE_BUSY between our own goroutines), at the cost of giving up
// WAL's free read concurrency. At TOP's metadata scale that cost is nil,
// and "every multi-step write in one BeginTx" is guaranteed by
// construction. Cross-process contention (topctl vs controller) is still
// covered by busy_timeout=5000 and surfaces as ErrLocked when it persists,
// so the API is identical if the split write-DB/read-pool layout is adopted
// later (see README for the reasoning).
//
// Downgrade protection: before migrating, the applied goose version is
// compared to SchemaVersionMax — a newer on-disk schema returns
// store.SchemaNewerError (never a silent downgrade).
func Open(ctx context.Context, path string) (store.Store, error) {
	if path == "" {
		return nil, fmt.Errorf("open state store: %w: empty path", domain.ErrInvalid)
	}
	if dir := filepath.Dir(path); dir != "." && path != ":memory:" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("open state store: %w", err)
		}
	}
	if path != ":memory:" {
		path = "file:" + path
	}
	db, err := sql.Open("sqlite", path+"?"+dsnParams)
	if err != nil {
		return nil, classify("open state store", err, "")
	}
	db.SetMaxOpenConns(1) // see Connection policy above
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, classify("open state store (ping "+path+")", err, "")
	}

	// The goose version table only exists once goose has run, so a missing
	// table means a fresh database (version 0).
	current, err := appliedVersion(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if current > SchemaVersionMax {
		_ = db.Close()
		return nil, store.SchemaNewerError{Current: current, Known: SchemaVersionMax}
	}

	// Migrations run inside transactions (goose wraps each plain-SQL file);
	// failure aborts startup loudly — no silent auto-rollback (ADR-0007).
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, migrations)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open state store: build migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open state store (migrate): %w", classify("migrate", err, ""))
	}

	return &Store{db: db}, nil
}

// Store is the store.Store implementation over one serial SQLite connection.
type Store struct {
	db *sql.DB
}

// SchemaVersion reports the current goose schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	return appliedVersion(ctx, s.db)
}

// Check verifies database integrity; callers must surface
// `topctl recover diagnose` rather than retrying on ErrCorrupt.
func (s *Store) Check(ctx context.Context) error {
	var result string
	err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result)
	if err != nil {
		return classify("integrity_check", err, "")
	}
	if result != "ok" {
		return domain.CorruptError{Detail: "integrity_check: " + result}
	}
	return nil
}

// Close implements store.Store.Close.
func (s *Store) Close() error { return s.db.Close() }

// appliedVersion reads the max applied goose version; 0 when the version
// table does not exist yet (fresh database).
func appliedVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).
		Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		if isNoSuchTable(err) {
			return 0, nil
		}
		return 0, classify("read schema version", err, "")
	}
	return v.Int64, nil
}

func isNoSuchTable(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
}
