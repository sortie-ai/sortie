ALTER TABLE run_history ADD COLUMN resume_input_tokens INTEGER;
ALTER TABLE run_history ADD COLUMN resume_output_tokens INTEGER;
ALTER TABLE run_history ADD COLUMN resume_cache_read_tokens INTEGER;
ALTER TABLE run_history ADD COLUMN resume_cache_write_tokens INTEGER;
