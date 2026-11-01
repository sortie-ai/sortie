CREATE TABLE posted_comments (
    issue_id   TEXT NOT NULL,
    comment_id TEXT NOT NULL,
    PRIMARY KEY (issue_id, comment_id)
);
