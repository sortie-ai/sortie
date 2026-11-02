ALTER TABLE run_history ADD COLUMN last_prompt_at TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN last_prompt_at TEXT NOT NULL DEFAULT '';
