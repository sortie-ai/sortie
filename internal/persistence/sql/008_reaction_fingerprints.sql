-- Migration 8: Cross-restart reaction deduplication
--
-- Tracks fingerprints for each (issue, reaction-kind) pair so the
-- orchestrator can detect already-dispatched reactions after a restart.
-- dispatched is 1 when a fix dispatch has been sent for this fingerprint,
-- 0 when only recorded. Upserts reset dispatched to 0 when the
-- fingerprint value changes.

CREATE TABLE reaction_fingerprints (
    issue_id    TEXT    NOT NULL,
    kind        TEXT    NOT NULL,
    fingerprint TEXT    NOT NULL,
    dispatched  INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL,
    PRIMARY KEY (issue_id, kind)
);
