-- Migration 5: Add turns_completed column to run history

ALTER TABLE run_history ADD COLUMN turns_completed INTEGER NOT NULL DEFAULT 0;
