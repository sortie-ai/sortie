-- Append-only: the newest row of a subject is the answer the gate last applied to it.
CREATE TABLE reaction_triage_outcomes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    issue_id    TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    fingerprint TEXT    NOT NULL,
    disposition TEXT    NOT NULL,
    fallback    TEXT    NOT NULL,
    elapsed_ms  INTEGER NOT NULL,
    chain_id    TEXT    NOT NULL,
    recorded_at TEXT    NOT NULL
);

CREATE INDEX idx_reaction_triage_outcomes_subject
    ON reaction_triage_outcomes(issue_id, kind, fingerprint);
