-- First-class build records and D17/D21 deployment runtime state.
--
-- A build is one attempt at an exact source commit, with logs carried by its
-- operation events. Releases link to the build that produced them. Deployments
-- record the per-process containers and dynamic loopback ports they created, so
-- the routes renderer reads Caddy upstreams from the serving deployment rather
-- than the legacy stable port.

CREATE TABLE eregion_platform_builds (
    id text PRIMARY KEY CHECK (id ~ '^bld_[0-9a-f]{32}$'),
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL,
    release_id text REFERENCES eregion_platform_releases(id) ON DELETE SET NULL,
    source jsonb NOT NULL DEFAULT '{}'::jsonb,
    commit_sha text NOT NULL DEFAULT ''
        CHECK (commit_sha = '' OR commit_sha ~ '^[0-9a-f]{40}$' OR commit_sha ~ '^[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','running','succeeded','failed','cancelled')),
    error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error')),
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    started_at timestamptz,
    completed_at timestamptz
);

CREATE INDEX eregion_platform_builds_environment
    ON eregion_platform_builds (environment_id, created_at DESC);

ALTER TABLE eregion_platform_releases
    ADD COLUMN build_id text REFERENCES eregion_platform_builds(id) ON DELETE SET NULL,
    ADD COLUMN commit_sha text NOT NULL DEFAULT ''
        CHECK (commit_sha = '' OR commit_sha ~ '^[0-9a-f]{40}$' OR commit_sha ~ '^[0-9a-f]{64}$');

ALTER TABLE eregion_platform_deployments
    DROP CONSTRAINT IF EXISTS eregion_platform_deployments_action_check;
ALTER TABLE eregion_platform_deployments
    ADD CONSTRAINT eregion_platform_deployments_action_check
    CHECK (action IN ('deploy','rollback','restart')),
    ADD COLUMN previous_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    ADD COLUMN instances jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE eregion_platform_environments
    ADD COLUMN serving_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL;

-- Existing environments keep a functional route renderer after the upgrade:
-- the latest successful deployment for the serving release becomes the
-- serving deployment.
UPDATE eregion_platform_environments e
SET serving_deployment_id = (
    SELECT d.id
    FROM eregion_platform_deployments d
    WHERE d.environment_id = e.id
      AND d.state = 'succeeded'
      AND d.release_id = e.serving_release_id
    ORDER BY d.completed_at DESC NULLS LAST, d.created_at DESC
    LIMIT 1
)
WHERE e.serving_release_id IS NOT NULL
  AND e.serving_deployment_id IS NULL;
