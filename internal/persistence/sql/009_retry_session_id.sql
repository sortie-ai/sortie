-- Migration 9: Add session_id to retry_entries for cross-retry session resume
ALTER TABLE retry_entries ADD COLUMN session_id TEXT;
