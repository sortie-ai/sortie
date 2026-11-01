-- An empty declared_outcome marks a run that ended on no declaration, and a row written before the column existed.

ALTER TABLE run_history ADD COLUMN declared_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN agent_text TEXT NOT NULL DEFAULT '';
