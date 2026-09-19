-- Migration 19: unaccounted-turn count on run_history
--
-- Counts the run's turns that issued a model request and produced no
-- token figure from any source: spend that occurred at an unknown
-- amount. It contributes no number to the four token columns, so a
-- non-zero count makes a row's total a lower bound on what the run
-- really cost. Existing rows default to zero, which is what every
-- writer before this migration recorded in effect.

ALTER TABLE run_history ADD COLUMN unaccounted_turns INTEGER NOT NULL DEFAULT 0;
