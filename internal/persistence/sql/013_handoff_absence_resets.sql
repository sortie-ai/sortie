-- Migration 13: reset points for the consecutive handoff-absence sequence
--
-- Records, per issue, the run_history id at which a work-observed verdict
-- ended that issue's absence sequence. Absence rows at or below that id are
-- excluded from the consecutive count, so only a work-observed verdict resets
-- the sequence. No verdict is stored on run_history: a terminal status of
-- 'succeeded' does not by itself assert that work was observed.

CREATE TABLE handoff_absence_resets (
    issue_id     TEXT    PRIMARY KEY,
    reset_run_id INTEGER NOT NULL,
    updated_at   TEXT    NOT NULL
);
