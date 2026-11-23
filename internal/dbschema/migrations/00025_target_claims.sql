-- +goose Up

-- Retain identities after release: a committed token is never reusable.
-- Operation IDs are retained as values rather than foreign keys because
-- operation retention must not erase claim tombstones.
CREATE TABLE eregion_target_claims (
    token text PRIMARY KEY CHECK (token <> ''),
    operation_id text NOT NULL CHECK (operation_id <> ''),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    sealed_at timestamptz,
    released_at timestamptz,
    CHECK (released_at IS NULL OR sealed_at IS NOT NULL)
);

CREATE TABLE eregion_target_claim_targets (
    target text PRIMARY KEY CHECK (target <> ''),
    token text NOT NULL REFERENCES eregion_target_claims(token)
);

-- A sealed claim still owns its targets until proof permits release.
CREATE UNIQUE INDEX eregion_target_claims_unreleased_operation
    ON eregion_target_claims(operation_id) WHERE released_at IS NULL;
CREATE INDEX eregion_target_claim_targets_token ON eregion_target_claim_targets(token);

-- Retirement is not successful execution. Keep its exact identity separately
-- from the step result so cancellation cannot be mistaken for completion.
CREATE TABLE eregion_retired_build_effects (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    step text NOT NULL,
    execution_id text NOT NULL,
    inputs_hash text NOT NULL,
    PRIMARY KEY (operation_id, step)
);

CREATE TABLE eregion_resource_dispatch_nodes (
    operation_id text PRIMARY KEY REFERENCES eregion_operations(id),
    node_name text NOT NULL,
    node_identity text NOT NULL
);

CREATE TABLE eregion_deployment_health_dispatches (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    step text NOT NULL,
    node_name text NOT NULL,
    node_identity text NOT NULL,
    attempt bigint NOT NULL,
    execution_id text NOT NULL,
    inputs_hash text NOT NULL,
    PRIMARY KEY (operation_id, step)
);
