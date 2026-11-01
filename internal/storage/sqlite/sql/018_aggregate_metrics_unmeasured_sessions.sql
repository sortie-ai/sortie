ALTER TABLE aggregate_metrics ADD COLUMN unmeasured_sessions INTEGER NOT NULL DEFAULT 0;

UPDATE aggregate_metrics
SET unmeasured_sessions = (SELECT COUNT(*) FROM run_history WHERE tokens_measured = 0)
WHERE key = 'agent_totals';
