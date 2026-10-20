CREATE TABLE eregion_operation_steps (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    step text NOT NULL CHECK (step ~ '^[a-z0-9]+(-[a-z0-9]+)*-v[0-9]+$'),
    inputs_hash text NOT NULL CHECK (inputs_hash ~ '^[0-9a-f]{64}$'),
    state text NOT NULL CHECK (state IN ('running','completed','failed')),
    attempt integer NOT NULL CHECK (attempt >= 0),
    execution_id text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    error text NOT NULL DEFAULT '',
    result jsonb,
    PRIMARY KEY (operation_id, step)
);

CREATE INDEX eregion_operation_steps_incomplete
    ON eregion_operation_steps (operation_id) WHERE state <> 'completed';
