-- +goose Up

CREATE TABLE eregion_definition_runs (
    operation_id text PRIMARY KEY REFERENCES eregion_operations(id),
    plan_nonce text NOT NULL UNIQUE,
    environment_id text NOT NULL,
    approved jsonb NOT NULL,
    next_step integer NOT NULL,
    failure_code text NOT NULL,
    failure_message text NOT NULL,
    finished_at timestamptz
);

CREATE UNIQUE INDEX eregion_definition_runs_active_environment ON eregion_definition_runs(environment_id) WHERE finished_at IS NULL;

CREATE TABLE eregion_definition_steps (
    parent_operation_id text NOT NULL REFERENCES eregion_definition_runs(operation_id),
    ordinal integer NOT NULL,
    kind text NOT NULL,
    target text NOT NULL,
    input jsonb NOT NULL,
    domain_id text,
    allocated_domain_id text UNIQUE,
    dns_plan jsonb,
    dns_cleanup jsonb,
    child_operation_id text UNIQUE REFERENCES eregion_operations(id) DEFERRABLE INITIALLY DEFERRED,
    dns_progress jsonb NOT NULL,
    PRIMARY KEY (parent_operation_id, ordinal)
);
