-- Migration 24: stage chain identifier and the result of a hop decision
--
-- chain_id groups the runs of one pass through a chain of stages. It is
-- frozen with a dispatch's selection, so retry_entries and stage_hops carry
-- it to the next dispatch. stage_target and stage_result record the hop
-- decision the run's exit reached, and both are empty when it reached none.
-- Rows written earlier keep empty values: nothing is backfilled.

ALTER TABLE run_history ADD COLUMN chain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN stage_target TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN stage_result TEXT NOT NULL DEFAULT '';
ALTER TABLE retry_entries ADD COLUMN chain_id TEXT NOT NULL DEFAULT '';
ALTER TABLE stage_hops ADD COLUMN chain_id TEXT NOT NULL DEFAULT '';
