-- Migration 14: issues held out of primary dispatch until a human acts
--
-- One row per parked issue. The row is deleted when the park is lifted,
-- so the table holds current state rather than history. parked_state is
-- the tracker state observed when the park was recorded. label is the
-- parking label the orchestrator applied. label_applied is 1 once the
-- orchestrator has confirmed the label is on the issue.

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
