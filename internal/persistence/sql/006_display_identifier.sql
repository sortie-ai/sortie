-- Migration 6: Add display identifier to run history

ALTER TABLE run_history ADD COLUMN display_identifier TEXT;
