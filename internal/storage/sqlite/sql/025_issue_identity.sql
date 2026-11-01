ALTER TABLE retry_entries ADD COLUMN display_identifier TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN issue_title TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN issue_url TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN issue_title TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN issue_url TEXT NOT NULL DEFAULT '';
