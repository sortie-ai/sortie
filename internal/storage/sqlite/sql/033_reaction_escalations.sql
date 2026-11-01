CREATE TABLE reaction_escalations (
    issue_id           TEXT    NOT NULL,
    kind               TEXT    NOT NULL,
    subject            TEXT    NOT NULL DEFAULT '',
    escalated_at       TEXT,
    escalation_trigger TEXT    NOT NULL,
    attempts           INTEGER NOT NULL DEFAULT 0,
    pr_number          INTEGER NOT NULL,
    pr_owner           TEXT    NOT NULL,
    pr_repo            TEXT    NOT NULL,
    pr_branch          TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (issue_id, kind)
);
