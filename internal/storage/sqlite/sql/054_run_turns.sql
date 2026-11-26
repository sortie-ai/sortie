CREATE TABLE run_turns (
    run_id               INTEGER NOT NULL,
    seq                  INTEGER NOT NULL,
    ended_at             TEXT    NOT NULL,
    tokens_measured      INTEGER NOT NULL,
    tool_calls           INTEGER NOT NULL,
    errors               INTEGER NOT NULL,
    estimated_cost_usd   REAL,
    input_cost_usd       REAL,
    output_cost_usd      REAL,
    cache_read_cost_usd  REAL,
    cache_write_cost_usd REAL,
    PRIMARY KEY (run_id, seq)
);

CREATE INDEX idx_run_turns_ended_at ON run_turns (ended_at);

CREATE TABLE run_turn_models (
    run_id             INTEGER NOT NULL,
    seq                INTEGER NOT NULL,
    model              TEXT    NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL,
    cache_write_tokens INTEGER NOT NULL,
    PRIMARY KEY (run_id, seq, model)
);
