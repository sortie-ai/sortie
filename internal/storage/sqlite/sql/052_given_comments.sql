CREATE TABLE given_comments (
    issue_id        TEXT NOT NULL,
    conversation_id TEXT NOT NULL,
    source          TEXT NOT NULL,
    comment_id      TEXT NOT NULL,
    body_sha256     TEXT NOT NULL,
    channel         TEXT NOT NULL,
    given_at        TEXT NOT NULL,
    PRIMARY KEY (issue_id, conversation_id, source, comment_id)
);
