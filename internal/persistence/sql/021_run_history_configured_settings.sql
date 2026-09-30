-- Migration 21: configured model, configured effort, and reported model
-- on run_history
--
-- Records the model and reasoning level the attempt was configured with
-- beside the model the runtime reported running. Empty means unset:
-- rows written before this migration read '' in all three columns.

ALTER TABLE run_history ADD COLUMN configured_model TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN configured_effort TEXT NOT NULL DEFAULT '';
ALTER TABLE run_history ADD COLUMN reported_model TEXT NOT NULL DEFAULT '';
