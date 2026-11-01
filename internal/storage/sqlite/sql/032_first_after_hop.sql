-- The lowest id of a hop is its first run: an issue runs one run at a time.
ALTER TABLE run_history ADD COLUMN first_after_hop INTEGER NOT NULL DEFAULT 0;

UPDATE run_history SET first_after_hop = 1
WHERE stage_previous <> ''
  AND chain_id <> ''
  AND NOT EXISTS (
    SELECT 1 FROM run_history p
    WHERE p.issue_id = run_history.issue_id
      AND p.chain_id = run_history.chain_id
      AND p.rule_name = run_history.rule_name
      AND p.stage_previous = run_history.stage_previous
      AND p.id < run_history.id);
