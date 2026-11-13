-- +goose Up

CREATE TABLE eregion_workspace (
    singleton boolean PRIMARY KEY,
    workspace_id text NOT NULL UNIQUE,
    name text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    registry_host_node text,
    registry_host_key_type text,
    registry_host_key_fingerprint text,
    registry_host_state_dir text
);
