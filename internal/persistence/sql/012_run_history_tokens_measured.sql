-- Migration 12: record whether a run's token figures are a measurement
ALTER TABLE run_history ADD COLUMN tokens_measured INTEGER NOT NULL DEFAULT 1;
