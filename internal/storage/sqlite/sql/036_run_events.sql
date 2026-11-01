-- Nullable: '' would misstate a pre-column row as a run with no dispatch.
ALTER TABLE run_history ADD COLUMN dispatch_id TEXT;

CREATE TABLE run_events (
    dispatch_id  TEXT    NOT NULL,
    seq          INTEGER NOT NULL,
    at           TEXT    NOT NULL,
    event        TEXT    NOT NULL,
    completed_at TEXT    NOT NULL,
    size         INTEGER NOT NULL,
    payload      TEXT    NOT NULL,
    PRIMARY KEY (dispatch_id, seq)
);

CREATE INDEX idx_run_events_completed_at ON run_events (completed_at, dispatch_id, size);

CREATE TABLE run_events_total (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    bytes INTEGER NOT NULL
);

INSERT INTO run_events_total (id, bytes) VALUES (1, 0);
