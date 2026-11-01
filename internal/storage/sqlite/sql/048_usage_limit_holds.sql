CREATE TABLE usage_limit_holds (
    agent_kind    TEXT    NOT NULL,
    model_only    INTEGER NOT NULL,
    model         TEXT    NOT NULL,
    resets_at_ms  INTEGER NOT NULL,
    held_since_ms INTEGER NOT NULL,
    PRIMARY KEY (agent_kind, model_only, model)
);

ALTER TABLE retry_entries ADD COLUMN usage_limit_notified INTEGER NOT NULL DEFAULT 0;
