-- D21: the release command is release-scoped. One row records the outcome of
-- the command digest for a release, so restart, scale and rollback can refuse
-- or skip it and a re-deploy after failure runs it again.

CREATE TABLE eregion_platform_release_command_runs (
    release_id text NOT NULL REFERENCES eregion_platform_releases(id) ON DELETE CASCADE,
    command_digest text NOT NULL CHECK (command_digest ~ '^[0-9a-f]{64}$'),
    operation_id text NOT NULL REFERENCES eregion_operations(id) ON DELETE CASCADE,
    state text NOT NULL CHECK (state IN ('running','succeeded','failed')),
    error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error')),
    error text NOT NULL DEFAULT '',
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    PRIMARY KEY (release_id, command_digest)
);

CREATE INDEX eregion_platform_release_command_runs_operation
    ON eregion_platform_release_command_runs (operation_id);
