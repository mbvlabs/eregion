-- +goose Up

CREATE TABLE eregion_platform_container_cleanup (
    container_name text PRIMARY KEY,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    node text NOT NULL,
    deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    reason text NOT NULL,
    attempts integer NOT NULL,
    last_error text NOT NULL,
    created_at timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL
);

CREATE INDEX eregion_platform_container_cleanup_due ON eregion_platform_container_cleanup (next_attempt_at, container_name);
