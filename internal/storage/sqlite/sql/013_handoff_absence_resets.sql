CREATE TABLE handoff_absence_resets (
    issue_id     TEXT    PRIMARY KEY,
    reset_run_id INTEGER NOT NULL,
    updated_at   TEXT    NOT NULL
);
