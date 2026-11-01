CREATE TABLE retry_entries (
    issue_id   TEXT    PRIMARY KEY,
    identifier TEXT    NOT NULL,
    attempt    INTEGER NOT NULL,
    due_at_ms  INTEGER NOT NULL,
    error      TEXT
);

CREATE TABLE run_history (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id      TEXT    NOT NULL,
    identifier    TEXT    NOT NULL,
    attempt       INTEGER NOT NULL,
    agent_adapter TEXT    NOT NULL,
    workspace     TEXT    NOT NULL,
    started_at    TEXT    NOT NULL,
    completed_at  TEXT    NOT NULL,
    status        TEXT    NOT NULL,
    error         TEXT
);

CREATE TABLE session_metadata (
    issue_id      TEXT    PRIMARY KEY,
    session_id    TEXT    NOT NULL,
    agent_pid     TEXT,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens  INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT    NOT NULL
);

CREATE TABLE aggregate_metrics (
    key             TEXT    PRIMARY KEY,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens    INTEGER NOT NULL DEFAULT 0,
    seconds_running REAL    NOT NULL DEFAULT 0,
    updated_at      TEXT    NOT NULL
);
