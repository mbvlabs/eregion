-- +goose Up

CREATE TABLE eregion_access_grants (
    id text PRIMARY KEY,
    record jsonb NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE eregion_integration_audit (
    id bigint PRIMARY KEY,
    entry jsonb NOT NULL,
    recorded_at timestamptz NOT NULL
);
