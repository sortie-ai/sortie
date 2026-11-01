-- Timestamps vary in precision and offset, so they compare through unixepoch.
ALTER TABLE parked_issues ADD COLUMN rule_name TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN agent_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN configured_model TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN stage_previous TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN stage_previous_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN chain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN dispatched_at TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN issue_type TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN issue_labels TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN issue_type TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN issue_labels TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN issue_state TEXT NOT NULL DEFAULT '';

UPDATE parked_issues SET
    rule_name              = r.rule_name,
    agent_kind             = r.agent_adapter,
    configured_model       = r.configured_model,
    stage_previous         = r.stage_previous,
    stage_previous_outcome = r.stage_previous_outcome,
    chain_id               = r.chain_id,
    dispatched_at          = r.dispatched_at
FROM run_history r
WHERE unixepoch(parked_issues.parked_at) IS NOT NULL
  AND r.id = (
    SELECT p.id FROM run_history p
    WHERE p.issue_id = parked_issues.issue_id
      AND p.agent_adapter <> ''
      AND unixepoch(p.started_at) IS NOT NULL
      AND unixepoch(p.started_at, 'subsec') <= unixepoch(parked_issues.parked_at, 'subsec')
    ORDER BY p.id DESC
    LIMIT 1);
