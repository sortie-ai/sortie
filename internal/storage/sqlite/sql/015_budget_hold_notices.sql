CREATE TABLE budget_hold_notices (
    issue_id   TEXT PRIMARY KEY,
    reason     TEXT NOT NULL,
    noticed_at TEXT NOT NULL
);
