-- +goose Up

CREATE TABLE eregion_platform_deployment_owners (
    environment_id text PRIMARY KEY REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    operation_id text REFERENCES eregion_operations(id) ON DELETE CASCADE,
    deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE CASCADE,
    owner_attempt integer,
    phase text NOT NULL,
    route_target_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    restore_release_id text REFERENCES eregion_platform_releases(id),
    restore_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX eregion_platform_deployment_owners_deployment ON eregion_platform_deployment_owners (deployment_id);
