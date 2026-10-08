-- Migration 23: automatic stage hops and the stage a dispatch followed
--
-- One stage_hops row per issue holds the current state since the last
-- reset: the consecutive hop count, the latest hop, and whether a read has
-- shown the label that hop added (target_observed). The row is deleted on
-- reset, so the table holds current state rather than history.
-- stage_previous and stage_previous_outcome freeze the rule that led to a
-- dispatch so a retry or run recovered after a restart renders the same
-- values.

CREATE TABLE stage_hops (
    issue_id           TEXT    PRIMARY KEY,
    identifier         TEXT    NOT NULL,
    hop_count          INTEGER NOT NULL,
    source_rule        TEXT    NOT NULL,
    target_rule        TEXT    NOT NULL,
    target_label       TEXT    NOT NULL,
    previous_outcome   TEXT    NOT NULL,
    source_dispatch_id TEXT    NOT NULL,
    target_observed    INTEGER NOT NULL DEFAULT 0,
    hopped_at          TEXT    NOT NULL
);

ALTER TABLE retry_entries ADD COLUMN stage_previous TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN stage_previous_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN stage_previous TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN stage_previous_outcome TEXT NOT NULL DEFAULT '';
