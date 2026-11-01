CREATE TABLE reaction_handoffs (
    issue_id   TEXT NOT NULL,
    kind       TEXT NOT NULL,
    comment_id TEXT NOT NULL,
    PRIMARY KEY (issue_id, kind, comment_id)
);
