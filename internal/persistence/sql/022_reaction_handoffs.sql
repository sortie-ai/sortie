-- Migration 22: review comments each issue's runs were given
--
-- One row per (issue, reaction kind, comment) a run's first prompt
-- presented when the run exited normally. Rows are only ever added: the
-- set outlives watch windows, escalations, terminal release, and
-- restarts, so a comment the agent already received is never handed over
-- as new a second time.

CREATE TABLE reaction_handoffs (
    issue_id   TEXT NOT NULL,
    kind       TEXT NOT NULL,
    comment_id TEXT NOT NULL,
    PRIMARY KEY (issue_id, kind, comment_id)
);
