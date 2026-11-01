CREATE TABLE parked_issues (
    issue_id      TEXT    PRIMARY KEY,
    identifier    TEXT    NOT NULL,
    display_id    TEXT,
    reason        TEXT    NOT NULL,
    parked_state  TEXT    NOT NULL DEFAULT '',
    label         TEXT    NOT NULL,
    label_applied INTEGER NOT NULL DEFAULT 0,
    parked_at     TEXT    NOT NULL
);
