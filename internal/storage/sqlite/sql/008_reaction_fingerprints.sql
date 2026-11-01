CREATE TABLE reaction_fingerprints (
    issue_id    TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    fingerprint TEXT    NOT NULL,
    dispatched  INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL,
    PRIMARY KEY (issue_id, kind)
);
