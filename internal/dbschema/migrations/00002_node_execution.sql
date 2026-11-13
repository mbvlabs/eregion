-- +goose Up

CREATE TABLE eregion_operation_events (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    sequence bigint NOT NULL,
    event jsonb NOT NULL,
    PRIMARY KEY (operation_id, sequence)
);

CREATE TABLE eregion_operation_steps (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    step text NOT NULL,
    inputs_hash text NOT NULL,
    state text NOT NULL,
    attempt integer NOT NULL,
    execution_id text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    error text NOT NULL,
    result jsonb,
    error_code text NOT NULL,
    PRIMARY KEY (operation_id, step)
);

CREATE INDEX eregion_operation_steps_incomplete ON eregion_operation_steps (operation_id)
    WHERE state <> 'completed';
