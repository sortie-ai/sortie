ALTER TABLE run_history ADD COLUMN agent_blocked INTEGER NOT NULL DEFAULT 0;

CREATE TABLE live_passes (
    issue_id       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    chain_id       TEXT NOT NULL,
    rule_name      TEXT NOT NULL,
    stage_previous TEXT NOT NULL,
    PRIMARY KEY (issue_id, kind)
);
