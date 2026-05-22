-- Migration 10: Add dispatch rule routing columns to retry_entries and run_history
ALTER TABLE retry_entries ADD COLUMN rule_name TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN template_id TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN agent_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN rule_name TEXT NOT NULL DEFAULT '';
