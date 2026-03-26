-- Migration 2: Extended token metrics for per-session and aggregate tracking

ALTER TABLE session_metadata ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE session_metadata ADD COLUMN model_name TEXT NOT NULL DEFAULT '';
ALTER TABLE session_metadata ADD COLUMN api_request_count INTEGER NOT NULL DEFAULT 0;

ALTER TABLE aggregate_metrics ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
