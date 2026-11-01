-- Nullable: '' would misstate an unrecorded origin as no trigger or a local run.
ALTER TABLE run_history ADD COLUMN trigger TEXT;
ALTER TABLE run_history ADD COLUMN reaction_kind TEXT;
ALTER TABLE run_history ADD COLUMN session_id TEXT;
ALTER TABLE run_history ADD COLUMN ssh_host TEXT;
CREATE INDEX idx_run_history_identifier ON run_history(identifier);
ALTER TABLE retry_entries ADD COLUMN origin_trigger TEXT NOT NULL DEFAULT '';

UPDATE retry_entries SET origin_trigger = trigger WHERE trigger NOT IN ('timer', '');
