-- +goose Up

CREATE TABLE eregion_operations (
    id text PRIMARY KEY,
    kind text NOT NULL,
    target text NOT NULL,
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    idempotency_key text NOT NULL,
    input_hash text NOT NULL,
    input jsonb NOT NULL,
    state text NOT NULL,
    attempt integer NOT NULL,
    completed_steps integer NOT NULL,
    last_sequence bigint NOT NULL,
    job_id bigint UNIQUE,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    error_code text NOT NULL,
    UNIQUE (actor_kind, actor_subject, kind, target, idempotency_key)
);

CREATE INDEX eregion_operations_unfinished ON eregion_operations (job_id)
    WHERE state IN ('queued','running','cancelling');
