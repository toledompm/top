// Package store defines the StateStore API implemented by internal/store/sqlite
// (state-store contract, ADR-0007). It names the surface callers receive —
// the traits, types, and error semantics come from internal/domain and
// docs/contracts/state-store.md; the interface deliberately contains ONLY
// what the single v0 SQLite implementation provides.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
)

// Store is TOP's metadata persistence surface — one implementation in v0
// (SQLite). Multi-step mutations succeed or fail atomically; error classes
// are the contract sentinels: ErrConflict/ErrLocked/ErrCorrupt from
// internal/domain, ErrNotFound, ErrInvalid, and SchemaNewer.
type Store interface {
	// --- schema / lifecycle -------------------------------------------------

	// SchemaVersion reports the current goose schema version. The caller
	// uses it to refuse running on a newer schema (downgrade protection).
	SchemaVersion(ctx context.Context) (int64, error)
	// Check verifies low-level integrity (PRAGMA integrity_check); on
	// failure it returns an ErrCorrupt-classified error — never retried.
	Check(ctx context.Context) error
	// Close releases the database connection pool.
	Close() error

	// --- operations (append-only history) -----------------------------------

	// InsertOperation records a new pending operation; duplicate IDs and
	// invalid records are ErrConflict/ErrInvalid.
	InsertOperation(ctx context.Context, op *domain.Operation) error
	// UpdateOperationState moves an operation through the lifecycle
	// (state-store contract: append-only, valid transitions only). A
	// transition the domain state machine rejects is ErrConflict; moving to
	// failed requires a non-empty redactedErr. at sets started/finished.
	UpdateOperationState(ctx context.Context, id uuid.UUID, next domain.OperationState, redactedErr string, at time.Time) error
	// ListOperations returns the most recent operations, newest first,
	// filtered by the (possibly zero) filter.
	ListOperations(ctx context.Context, limit int, filter *OperationFilter) ([]*domain.Operation, error)

	// --- observed state ------------------------------------------------------

	// UpsertObserved stores the last backend-reported snapshot for one
	// service with the revision it was inspected at.
	UpsertObserved(ctx context.Context, ref domain.ServiceRef, obs *domain.ObservedState, revision string, at time.Time) error
	// GetObserved returns the stored snapshot, its revision, and the
	// inspection time; ErrNotFound when the service has never been observed.
	GetObserved(ctx context.Context, ref domain.ServiceRef) (*domain.ObservedState, string, time.Time, error)

	// --- checkpoints ---------------------------------------------------------

	// SetCheckpoint records reconciliation resume/rollback state.
	SetCheckpoint(ctx context.Context, cp *Checkpoint) error
	// GetCheckpoint returns the current checkpoint (zero-valued when none).
	GetCheckpoint(ctx context.Context) (*Checkpoint, error)

	// --- webhook delivery dedup ---------------------------------------------

	// RecordDelivery inserts the delivery id; true = first time seen,
	// false = duplicate (webhook dedup, state-store contract).
	RecordDelivery(ctx context.Context, deliveryID string, at time.Time) (bool, error)

	// --- reconciliation runs (atomic composite write) -------------------------

	// RecordReconcileRun writes a run summary and its per-service outcomes
	// in one transaction, advancing the per-service applied-revision map for
	// services whose outcome succeeded or was a noop; when cp is non-nil it
	// also replaces the checkpoint atomically with the run.
	RecordReconcileRun(ctx context.Context, run *domain.ReconciliationRun, cp *Checkpoint) error

	// --- resource ledger (ADR-0009) ------------------------------------------

	// InsertLedgerEntry records a host resource TOP created. A duplicate
	// (kind, identifier) is ErrConflict.
	InsertLedgerEntry(ctx context.Context, r *Resource) error
	// MarkManaged transitions a ledger entry to state managed (the
	// bootstrap-owned → managed handoff, ADR-0009).
	MarkManaged(ctx context.Context, kind, identifier string) error
	// ListLedger returns ledger entries, optionally filtered by kind
	// ("" = every kind).
	ListLedger(ctx context.Context, kind string) ([]*Resource, error)
	// RemoveLedgerEntry deletes a ledger entry after teardown of the
	// resource; ErrNotFound when absent.
	RemoveLedgerEntry(ctx context.Context, kind, identifier string) error

	// --- backup registry -----------------------------------------------------

	// InsertBackup registers a backup file in the registry; duplicate
	// paths are ErrConflict.
	InsertBackup(ctx context.Context, b *Backup) error
	// ListBackups returns the registry, newest first.
	ListBackups(ctx context.Context) ([]*Backup, error)
	// SetBackupVerified records the verification result and time for one
	// backup; ErrNotFound when the path is unregistered.
	SetBackupVerified(ctx context.Context, path, verifyResult string, at time.Time) error
	// RemoveBackup deletes one registry row after the file itself is
	// pruned (retention, backup-recovery contract); ErrNotFound when the
	// path is unregistered. The engine deletes the file FIRST, then the
	// row, so a registry row is never present for a missing file.
	RemoveBackup(ctx context.Context, path string) error

	// --- serialization lease ---------------------------------------------------

	// AcquireLease takes (or renews) the named serialization lease for
	// holderID. When another holder owns an un-expired lease it returns
	// ErrLocked carrying the holder identity. renew=false keeps the current
	// expiry unchanged when holderID already holds it (idempotent
	// acquire); renew=true extends the expiry by ttl from now.
	AcquireLease(ctx context.Context, name, holderID string, ttl time.Duration, renew bool) (bool, error)
	// ReleaseLease removes the named lease held by holderID; releasing an
	// un-expired lease held by someone else is ErrLocked.
	ReleaseLease(ctx context.Context, name, holderID string) error
}

// Filter/ledger/backup value types -------------------------------------------------

// OperationFilter narrows ListOperations; zero fields match everything.
type OperationFilter struct {
	// State: only operations currently in this state.
	State domain.OperationState
	// Kind: only operations of this kind.
	Kind domain.OperationKind
	// CorrelationID: only operations caused by this correlation id.
	CorrelationID string
}

// ResourceState is the lease lifecycle ADR-0009 ledger discipline gives a
// recorded resource: bootstrap-owned at install, managed after adoption,
// pre-existing for inventoried-but-not-created resources.
type ResourceState string

const (
	ResourceBootstrapOwned ResourceState = "bootstrap-owned"
	ResourceManaged        ResourceState = "managed"
	ResourcePreExisting    ResourceState = "pre-existing"
)

// IsValid reports whether s is a known ledger resource state.
func (s ResourceState) IsValid() bool {
	switch s {
	case ResourceBootstrapOwned, ResourceManaged, ResourcePreExisting:
		return true
	}
	return false
}

// String implements fmt.Stringer.
func (s ResourceState) String() string { return string(s) }

// Resource is one host resource TOP can positively identify via the
// ownership ledger (ADR-0009 layer 3).
type Resource struct {
	Kind        string // e.g. "app", "network", "volume"
	Identifier  string
	Labels      map[string]string // top.dev/* labels
	CreatedByOp string            // operation ID that created it
	State       ResourceState
	CreatedAt   time.Time
}

// Backup is one registered metadata backup file (ADR-0007 /
// backup-recovery contract — restore-support registry only; M5 uses it).
type Backup struct {
	Path         string
	Size         int64
	WatermarkOp  string // newest operation ID included in the backup
	VerifiedAt   time.Time
	VerifyResult string // e.g. "ok", failure detail
	CreatedAt    time.Time
}

// Checkpoint extends the reconciliation checkpoint (domain.Checkpoint) with
// the per-service *applied* revision map the state-store contract records:
// what each service's working baseline is, distinct from last-known-good.
type Checkpoint struct {
	// LastApplied is the revision of the last run that became the working
	// baseline (domain.Checkpoint semantics).
	LastApplied string
	// LastKnownGood is the repo-wide revision recorded when a run came out
	// entirely green ("" when none has yet).
	LastKnownGood string
	// LastKnownGoodPerService maps a service to the revision whose release
	// is its rollback target (domain.Checkpoint semantics).
	LastKnownGoodPerService map[domain.ServiceRef]domain.RevisionRef
	// AppliedPerService maps a service to the revision of what was last
	// applied to it (its working baseline).
	AppliedPerService map[domain.ServiceRef]string
}

// ErrSchemaNewer classifies the downgrade-protection failure
// (docs/contracts/state-store.md: the controller refuses to run on a newer
// schema than it understands; branch with errors.Is, or errors.As on
// SchemaNewerError for the versions).
var ErrSchemaNewer = errors.New("store schema is newer than this build")

// SchemaNewerError reports an on-disk goose schema version greater than
// SchemaVersionMax; running forward would be a silent downgrade. The
// recovery path is: run the newer version (or restore an older backup);
// automatic rollback is intentionally absent.
type SchemaNewerError struct {
	Current int64 // schema version found on disk
	Known   int64 // highest version this build migrates to
}

func (e SchemaNewerError) Error() string {
	return fmt.Sprintf(
		"store schema is newer than this build: schema version %d > known %d (hint: run a newer topctl/controller build, or restore an earlier backup; downgrade is never automatic)",
		e.Current, e.Known)
}

// Unwrap yields ErrSchemaNewer.
func (e SchemaNewerError) Unwrap() error { return ErrSchemaNewer }
