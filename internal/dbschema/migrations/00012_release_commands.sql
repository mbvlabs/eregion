-- +goose Up

CREATE TABLE eregion_platform_release_command_runs (
    release_id text NOT NULL REFERENCES eregion_platform_releases(id) ON DELETE CASCADE,
    command_digest text NOT NULL,
    operation_id text NOT NULL REFERENCES eregion_operations(id) ON DELETE CASCADE,
    state text NOT NULL,
    error_code text NOT NULL,
    error text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    PRIMARY KEY (release_id, command_digest)
);

CREATE INDEX eregion_platform_release_command_runs_operation ON eregion_platform_release_command_runs (operation_id);
