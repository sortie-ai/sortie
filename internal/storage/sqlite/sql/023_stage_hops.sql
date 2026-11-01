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
