-- NULL in both run_history columns marks a row written before provenance was kept, and a run without provenance has no amount rows.

ALTER TABLE run_history ADD COLUMN usage_provenance TEXT;
ALTER TABLE run_history ADD COLUMN usage_completeness TEXT;

CREATE TABLE run_reported_amounts (
    run_id      INTEGER NOT NULL,
    unit        TEXT    NOT NULL,
    model       TEXT    NOT NULL,
    amount      REAL    NOT NULL,
    lower_bound INTEGER NOT NULL,
    PRIMARY KEY (run_id, unit, model)
);
