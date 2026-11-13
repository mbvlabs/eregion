-- +goose Up

CREATE TABLE eregion_platform_releases (
    id text PRIMARY KEY,
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    image text NOT NULL,
    image_id text NOT NULL,
    source jsonb NOT NULL,
    build jsonb,
    command jsonb,
    status text NOT NULL,
    error_code text NOT NULL,
    error text NOT NULL,
    created_at timestamptz NOT NULL,
    completed_at timestamptz,
    build_id text,
    commit_sha text NOT NULL,
    pruned_at timestamptz
);

CREATE INDEX eregion_platform_releases_environment ON eregion_platform_releases (environment_id, created_at DESC);
