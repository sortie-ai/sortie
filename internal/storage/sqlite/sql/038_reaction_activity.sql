CREATE TABLE reaction_activity (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id    TEXT    NOT NULL,
    chain_id    TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    activity    TEXT    NOT NULL,
    recorded_at TEXT    NOT NULL
);

CREATE INDEX idx_reaction_activity_pass ON reaction_activity(issue_id, chain_id);

CREATE UNIQUE INDEX idx_reaction_activity_watch
    ON reaction_activity(issue_id, chain_id, kind) WHERE activity = 'watch';
