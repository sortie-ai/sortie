CREATE TABLE issue_outcomes (
    issue_id            TEXT    PRIMARY KEY,
    kind                TEXT    NOT NULL,
    observed_at         TEXT    NOT NULL,
    state               TEXT    NOT NULL DEFAULT '',
    merge_reaction_kind TEXT    NOT NULL DEFAULT '',
    pr_number           INTEGER NOT NULL DEFAULT 0,
    pr_repository       TEXT    NOT NULL DEFAULT ''
);
