CREATE TABLE eregion_operations (
    id text PRIMARY KEY CHECK (id ~ '^op_[0-9a-f]{32}$'),
    kind text NOT NULL CHECK (kind = 'diagnostic.test'),
    target text NOT NULL CHECK (target = 'control-plane'),
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    idempotency_key text NOT NULL,
    input_hash text NOT NULL,
    input jsonb NOT NULL,
    state text NOT NULL CHECK (state IN ('queued','running','cancelling','succeeded','failed','cancelled')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    completed_steps integer NOT NULL DEFAULT 0 CHECK (completed_steps >= 0),
    last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
    job_id bigint UNIQUE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (actor_kind, actor_subject, kind, target, idempotency_key)
);

CREATE TABLE eregion_operation_events (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    sequence bigint NOT NULL CHECK (sequence > 0),
    event jsonb NOT NULL,
    PRIMARY KEY (operation_id, sequence)
);

CREATE INDEX eregion_operations_unfinished ON eregion_operations (job_id)
    WHERE state IN ('queued','running','cancelling');
