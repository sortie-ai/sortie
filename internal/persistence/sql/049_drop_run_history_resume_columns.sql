ALTER TABLE run_history DROP COLUMN resume_input_tokens;
ALTER TABLE run_history DROP COLUMN resume_output_tokens;
ALTER TABLE run_history DROP COLUMN resume_cache_read_tokens;
ALTER TABLE run_history DROP COLUMN resume_cache_write_tokens;
ALTER TABLE run_history DROP COLUMN resume_model_usage;
