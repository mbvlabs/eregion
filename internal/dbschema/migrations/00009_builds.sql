-- +goose Up

CREATE TABLE eregion_platform_builds (
    id text PRIMARY KEY,
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL,
    release_id text REFERENCES eregion_platform_releases(id) ON DELETE SET NULL,
    source jsonb NOT NULL,
    commit_sha text NOT NULL,
    state text NOT NULL,
    error_code text NOT NULL,
    error text NOT NULL,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz
);

CREATE INDEX eregion_platform_builds_environment ON eregion_platform_builds (environment_id, created_at DESC);
