-- +goose Up

CREATE TABLE eregion_build_capacity_claims (
    operation_id text PRIMARY KEY REFERENCES eregion_operations(id),
    claim_id text NOT NULL UNIQUE DEFAULT gen_random_uuid()::text,
    node_name text NOT NULL REFERENCES eregion_platform_nodes(name),
    builder_identity text NOT NULL,
    attempt integer NOT NULL CHECK (attempt > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    checked_at timestamptz
);

CREATE INDEX eregion_build_capacity_claims_node ON eregion_build_capacity_claims(node_name);
