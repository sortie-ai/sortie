-- Migration 11: Add token columns to run_history for per-issue cost budgeting
ALTER TABLE run_history ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_history ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_history ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_history ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
