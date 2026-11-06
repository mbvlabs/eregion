-- Environment-owned generic Docker resources. One row is one declared
-- resource identity; the container is an observed effect, not the declaration.
--
-- The row keeps the desired specification (image, non-secret environment,
-- secret NAMES mapped to image environment variables, named-volume mounts and
-- an exec health check) and the last applied immutable image identity. Secret
-- values are never stored here: they stay in the vault and are resolved
-- transiently for one provision operation.
--
-- Destroying a resource removes its container but keeps this row and its named
-- volumes so data is retained and the same identity can be applied again.
-- Volume deletion is deliberately not implemented in migration 022.

CREATE TABLE eregion_platform_resources (
    id text PRIMARY KEY CHECK (id ~ '^res_[0-9a-f]{32}$'),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (name ~ '^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$'),
    node_name text NOT NULL REFERENCES eregion_platform_nodes(name),
    spec jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','provisioning','ready','degraded','destroying','destroyed')),
    image text NOT NULL DEFAULT '',
    image_id text NOT NULL DEFAULT ''
        CHECK (image_id = '' OR image_id ~ '^sha256:[0-9a-f]{64}$'),
    image_digest text NOT NULL DEFAULT '',
    container_name text NOT NULL DEFAULT '',
    network_name text NOT NULL DEFAULT '',
    volumes jsonb NOT NULL DEFAULT '[]'::jsonb,
    health_state text NOT NULL DEFAULT 'unknown'
        CHECK (health_state IN ('unknown','healthy','unhealthy')),
    health_detail text NOT NULL DEFAULT '',
    health_checked_at timestamptz,
    operation_id text REFERENCES eregion_operations(id),
    error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error')),
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    destroyed_at timestamptz,
    UNIQUE (environment_id, name)
);

CREATE INDEX eregion_platform_resources_environment
    ON eregion_platform_resources (environment_id, name);
CREATE INDEX eregion_platform_resources_app
    ON eregion_platform_resources (app_id, environment_id, name);
