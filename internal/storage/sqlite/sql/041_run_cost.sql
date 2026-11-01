-- A NULL cost_status on run_history marks a row not yet priced, which the next start fills.

ALTER TABLE run_history ADD COLUMN estimated_cost_usd REAL;
ALTER TABLE run_history ADD COLUMN cost_status TEXT;

ALTER TABLE run_model_usage ADD COLUMN estimated_cost_usd REAL;
ALTER TABLE run_model_usage ADD COLUMN cost_status TEXT;
ALTER TABLE run_model_usage ADD COLUMN price_source TEXT NOT NULL DEFAULT '';
ALTER TABLE run_model_usage ADD COLUMN price_table_version TEXT NOT NULL DEFAULT '';

ALTER TABLE session_metadata ADD COLUMN estimated_cost_usd REAL;
ALTER TABLE session_metadata ADD COLUMN cost_status TEXT NOT NULL DEFAULT '';
