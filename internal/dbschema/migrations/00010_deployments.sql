-- +goose Up

CREATE TABLE eregion_platform_deployments (
    id text PRIMARY KEY,
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    action text NOT NULL,
    release_id text NOT NULL REFERENCES eregion_platform_releases(id),
    previous_release_id text REFERENCES eregion_platform_releases(id),
    state text NOT NULL,
    error_code text NOT NULL,
    message text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL,
    secret_revisions jsonb NOT NULL,
    previous_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    instances jsonb NOT NULL
);

CREATE INDEX eregion_platform_deployments_environment ON eregion_platform_deployments (environment_id, created_at DESC);
