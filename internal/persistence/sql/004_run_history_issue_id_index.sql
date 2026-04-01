-- Migration 4: Index run_history by issue_id for efficient budget queries

CREATE INDEX idx_run_history_issue_id ON run_history(issue_id);
