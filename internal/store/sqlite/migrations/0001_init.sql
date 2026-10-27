-- 0001_init — TOP state-store schema v1 (state-store contract, ADR-0007).
-- goose owns the schema version (goose_db_version, created automatically).
-- Timestamps are Unix nanoseconds in UTC; NULL means "unset/zero time".
-- JSON columns are TEXT; redaction applies at the call site (ADR-0008),
-- never inside the store.

-- +goose Up
CREATE TABLE operations (
    id             TEXT PRIMARY KEY,
    created_at     INTEGER NOT NULL,
    kind           TEXT    NOT NULL,
    state          TEXT    NOT NULL,
    actor          TEXT    NOT NULL,
    correlation_id TEXT    NOT NULL DEFAULT '',
    revision       TEXT    NOT NULL DEFAULT '',
    started_at     INTEGER,
    finished_at    INTEGER,
    error          TEXT    NOT NULL DEFAULT '', -- redacted error text on failure
    details        TEXT    NOT NULL DEFAULT '{}' -- JSON structured payload
);

CREATE INDEX idx_operations_state ON operations(state);
CREATE INDEX idx_operations_kind_created ON operations(kind, created_at);

-- Per-run reconciliation summaries (ReconciliationRun), keyed by the
-- operation that performed the run.
CREATE TABLE runs (
    operation_id     TEXT PRIMARY KEY,
    actor            TEXT    NOT NULL,
    correlation_id   TEXT    NOT NULL DEFAULT '',
    revision         TEXT    NOT NULL,
    status           TEXT    NOT NULL,
    started_at       INTEGER NOT NULL,
    finished_at      INTEGER,
    validation_error TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_runs_correlation ON runs(correlation_id);

CREATE TABLE run_outcomes (
    operation_id TEXT    NOT NULL REFERENCES runs(operation_id) ON DELETE CASCADE,
    ref          TEXT    NOT NULL, -- ns/name
    result       TEXT    NOT NULL,
    changes      TEXT,             -- JSON []string
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    error        TEXT    NOT NULL DEFAULT '', -- redacted
    hint         TEXT    NOT NULL DEFAULT '',
    skip         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (operation_id, ref)
);

CREATE TABLE observed_state (
    ref         TEXT PRIMARY KEY, -- ns/name
    revision    TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL,    -- JSON ObservedState
    observed_at INTEGER NOT NULL
);

CREATE TABLE checkpoints (
    id                  INTEGER PRIMARY KEY CHECK (id = 1), -- singleton
    last_applied        TEXT NOT NULL DEFAULT '',
    last_known_good     TEXT NOT NULL DEFAULT '',
    last_known_good_per_service TEXT NOT NULL DEFAULT '{}', -- JSON map
    applied_per_service TEXT NOT NULL DEFAULT '{}',         -- JSON map
    updated_at          INTEGER NOT NULL
);

-- Webhook delivery dedup: INSERT OR IGNORE; zero rows = duplicate.
CREATE TABLE deliveries (
    delivery_id TEXT PRIMARY KEY,
    received_at INTEGER NOT NULL
);

-- Resource ledger (ADR-0009 layer 3): every host resource TOP created,
-- matched on (kind, identifier).
CREATE TABLE resources (
    kind          TEXT NOT NULL,
    identifier    TEXT NOT NULL,
    labels        TEXT NOT NULL DEFAULT '{}', -- JSON string map
    created_by_op TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL, -- bootstrap-owned | managed | pre-existing
    created_at    INTEGER NOT NULL,
    UNIQUE (kind, identifier)
);

CREATE TABLE backups (
    path          TEXT PRIMARY KEY,
    size          INTEGER NOT NULL DEFAULT 0,
    watermark_op  TEXT NOT NULL DEFAULT '',
    verified_at   INTEGER,
    verify_result TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL
);

CREATE TABLE leases (
    name        TEXT PRIMARY KEY,
    holder_id   TEXT NOT NULL,
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);

-- +goose Down
-- Forward-only per ADR-0007; retained for completeness of the goose
-- version list. Never invoked by TOP's controller or recovery path.
DROP TABLE leases;
DROP TABLE backups;
DROP TABLE resources;
DROP TABLE deliveries;
DROP TABLE checkpoints;
DROP TABLE observed_state;
DROP TABLE run_outcomes;
DROP TABLE runs;
DROP TABLE operations;
