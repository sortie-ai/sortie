-- Timestamps vary in precision and offset, so they compare through unixepoch.
ALTER TABLE retry_entries ADD COLUMN configured_model TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN issue_type TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN issue_labels TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN issue_state TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN dispatched_at TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN dispatched_at TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN display_identifier TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN issue_title TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN issue_url TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN dispatched_at TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN run_dispatched INTEGER NOT NULL DEFAULT 0;

UPDATE run_history SET dispatched_at = COALESCE((
    SELECT p.started_at FROM run_history p
    WHERE p.issue_id = run_history.issue_id
      AND p.chain_id = run_history.chain_id
      AND p.agent_adapter <> ''
      AND unixepoch(p.started_at) IS NOT NULL
    ORDER BY unixepoch(p.started_at, 'subsec'), p.id
    LIMIT 1), '')
WHERE chain_id <> '';

UPDATE retry_entries SET dispatched_at = COALESCE((
    SELECT p.started_at FROM run_history p
    WHERE p.issue_id = retry_entries.issue_id
      AND p.chain_id = retry_entries.chain_id
      AND p.agent_adapter <> ''
      AND unixepoch(p.started_at) IS NOT NULL
    ORDER BY unixepoch(p.started_at, 'subsec'), p.id
    LIMIT 1), '')
WHERE chain_id <> '';

UPDATE retry_entries SET configured_model = COALESCE((
    SELECT p.configured_model FROM run_history p
    WHERE p.issue_id = retry_entries.issue_id
      AND p.chain_id = retry_entries.chain_id
      AND p.agent_adapter <> ''
    ORDER BY p.id DESC
    LIMIT 1), '')
WHERE chain_id <> '';

UPDATE stage_hops SET dispatched_at = COALESCE((
    SELECT p.started_at FROM run_history p
    WHERE p.issue_id = stage_hops.issue_id
      AND p.chain_id = stage_hops.chain_id
      AND p.agent_adapter <> ''
      AND unixepoch(p.started_at) IS NOT NULL
    ORDER BY unixepoch(p.started_at, 'subsec'), p.id
    LIMIT 1), '')
WHERE chain_id <> '';

UPDATE stage_hops SET display_identifier = COALESCE((
    SELECT p.display_identifier FROM run_history p
    WHERE p.issue_id = stage_hops.issue_id
      AND (p.issue_title <> '' OR p.issue_url <> '')
    ORDER BY p.id DESC
    LIMIT 1), '');

UPDATE stage_hops SET issue_title = COALESCE((
    SELECT p.issue_title FROM run_history p
    WHERE p.issue_id = stage_hops.issue_id
      AND (p.issue_title <> '' OR p.issue_url <> '')
    ORDER BY p.id DESC
    LIMIT 1), '');

UPDATE stage_hops SET issue_url = COALESCE((
    SELECT p.issue_url FROM run_history p
    WHERE p.issue_id = stage_hops.issue_id
      AND (p.issue_title <> '' OR p.issue_url <> '')
    ORDER BY p.id DESC
    LIMIT 1), '');

UPDATE stage_hops SET run_dispatched = EXISTS (
    SELECT 1 FROM run_history p
    WHERE p.issue_id = stage_hops.issue_id
      AND p.agent_adapter <> ''
      AND unixepoch(p.started_at, 'subsec') >= unixepoch(stage_hops.hopped_at, 'subsec'))
