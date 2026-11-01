-- Timestamps vary in precision and offset, so they compare through unixepoch.
-- The handoff-withheld literal is persistence.HandoffAbsenceErrorPrefix.
ALTER TABLE parked_issues ADD COLUMN session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN template_id TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN statement TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN released_at TEXT NOT NULL DEFAULT '';
ALTER TABLE parked_issues ADD COLUMN release_gesture TEXT NOT NULL DEFAULT '';

UPDATE parked_issues SET
    statement   = r.agent_text,
    session_id  = CASE WHEN COALESCE(r.reaction_kind, '') = ''
                        AND (r.status = 'succeeded'
                             OR (r.status = 'failed' AND substr(COALESCE(r.error, ''), 1, 18) = 'handoff withheld: '))
                       THEN COALESCE(r.session_id, '') ELSE '' END,
    template_id = CASE WHEN COALESCE(r.reaction_kind, '') = ''
                        AND (r.status = 'succeeded'
                             OR (r.status = 'failed' AND substr(COALESCE(r.error, ''), 1, 18) = 'handoff withheld: '))
                       THEN r.template_id ELSE '' END
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
