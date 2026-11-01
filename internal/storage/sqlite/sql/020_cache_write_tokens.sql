-- Older rows read zero: earlier writers folded cache writes into input.
ALTER TABLE run_history ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE session_metadata ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE aggregate_metrics ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
