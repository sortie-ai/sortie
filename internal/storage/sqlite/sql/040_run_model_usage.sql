CREATE TABLE run_model_usage (
    run_id             INTEGER NOT NULL,
    model              TEXT    NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    PRIMARY KEY (run_id, model)
);

ALTER TABLE run_history ADD COLUMN resume_model_usage TEXT;
