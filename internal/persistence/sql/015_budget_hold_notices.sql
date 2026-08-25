-- Migration 15: cross-restart dedup for the budget hold tracker notice
--
-- One row per issue whose current budget hold has been announced on the
-- tracker. The row is deleted when the hold clears, evaluated on the
-- same evidence rule that prunes the in-memory announcement latch, or
-- when both budgets are disabled. noticed_at is the hold time the
-- posted notice reported, kept so an operator reading the database can
-- align a row with the comment on the issue, read by no runtime
-- decision.

CREATE TABLE budget_hold_notices (
    issue_id   TEXT PRIMARY KEY,
    reason     TEXT NOT NULL,
    noticed_at TEXT NOT NULL
);
