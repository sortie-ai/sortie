-- Migration 3: Add workflow filename to run history

ALTER TABLE run_history ADD COLUMN workflow_file TEXT;
