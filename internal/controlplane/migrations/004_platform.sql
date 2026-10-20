-- M2/M3 platform state: placement facts, apps, environments, releases,
-- deployments, and per-environment change history. PostgreSQL is authoritative;
-- River remains execution bookkeeping only.

ALTER TABLE eregion_operations DROP CONSTRAINT IF EXISTS eregion_operations_kind_check;
ALTER TABLE eregion_operations DROP CONSTRAINT IF EXISTS eregion_operations_target_check;

ALTER TABLE eregion_operations
    ADD CONSTRAINT eregion_operations_kind_check
    CHECK (kind ~ '^(diagnostic\.test|platform\.[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*)$');

ALTER TABLE eregion_operations
    ADD CONSTRAINT eregion_operations_target_check
    CHECK (length(target) BETWEEN 1 AND 200 AND target ~ '^[A-Za-z0-9][A-Za-z0-9._:/@-]*$');

CREATE TABLE eregion_platform_nodes (
    name text PRIMARY KEY CHECK (name ~ '^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$'),
    roles text[] NOT NULL CHECK (cardinality(roles) >= 1),
    capabilities text[] NOT NULL CHECK (cardinality(capabilities) >= 1),
    address text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'registered' CHECK (status IN ('registered','ready','unreachable','disabled')),
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE eregion_platform_apps (
    id text PRIMARY KEY CHECK (id ~ '^app_[0-9a-f]{32}$'),
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE eregion_platform_environments (
    id text PRIMARY KEY CHECK (id ~ '^env_[0-9a-f]{32}$'),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (name ~ '^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$'),
    node_name text NOT NULL REFERENCES eregion_platform_nodes(name),
    host_port integer NOT NULL UNIQUE CHECK (host_port BETWEEN 1 AND 65535),
    candidate_port integer NOT NULL UNIQUE CHECK (candidate_port BETWEEN 1 AND 65535),
    container_port integer NOT NULL CHECK (container_port BETWEEN 1 AND 65535),
    bind_address text NOT NULL DEFAULT '127.0.0.1',
    health_type text NOT NULL DEFAULT 'http' CHECK (health_type IN ('http','tcp')),
    health_path text NOT NULL DEFAULT '/',
    health_timeout_seconds integer NOT NULL DEFAULT 30 CHECK (health_timeout_seconds BETWEEN 1 AND 300),
    env jsonb NOT NULL DEFAULT '{}'::jsonb,
    secret_refs jsonb NOT NULL DEFAULT '{}'::jsonb,
    secret_revision bigint NOT NULL DEFAULT 0 CHECK (secret_revision >= 0),
    desired_release_id text,
    applied_release_id text,
    serving_release_id text,
    health_state text NOT NULL DEFAULT 'unknown' CHECK (health_state IN ('unknown','healthy','unhealthy')),
    health_detail text NOT NULL DEFAULT '',
    health_checked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (app_id, name),
    CHECK (host_port <> candidate_port)
);

CREATE TABLE eregion_platform_releases (
    id text PRIMARY KEY CHECK (id ~ '^rel_[0-9a-f]{32}$'),
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    image text NOT NULL CHECK (length(image) BETWEEN 1 AND 255),
    image_id text NOT NULL DEFAULT '',
    source jsonb NOT NULL,
    build jsonb,
    command jsonb,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','failed')),
    error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error')),
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz
);

ALTER TABLE eregion_platform_environments
    ADD CONSTRAINT eregion_platform_environments_desired_release_fk
    FOREIGN KEY (desired_release_id) REFERENCES eregion_platform_releases(id),
    ADD CONSTRAINT eregion_platform_environments_applied_release_fk
    FOREIGN KEY (applied_release_id) REFERENCES eregion_platform_releases(id),
    ADD CONSTRAINT eregion_platform_environments_serving_release_fk
    FOREIGN KEY (serving_release_id) REFERENCES eregion_platform_releases(id);

CREATE TABLE eregion_platform_deployments (
    id text PRIMARY KEY CHECK (id ~ '^dep_[0-9a-f]{32}$'),
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    action text NOT NULL CHECK (action IN ('deploy','rollback')),
    release_id text NOT NULL REFERENCES eregion_platform_releases(id),
    previous_release_id text REFERENCES eregion_platform_releases(id),
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','running','succeeded','failed','cancelled')),
    error_code text NOT NULL DEFAULT '' CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error')),
    message text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz
);

CREATE INDEX eregion_platform_deployments_environment
    ON eregion_platform_deployments (environment_id, created_at DESC);
CREATE INDEX eregion_platform_releases_environment
    ON eregion_platform_releases (environment_id, created_at DESC);

CREATE TABLE eregion_platform_changes (
    id text PRIMARY KEY CHECK (id ~ '^chg_[0-9a-f]{32}$'),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence > 0),
    kind text NOT NULL,
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    summary text NOT NULL,
    desired jsonb,
    applied jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (environment_id, sequence),
    UNIQUE (operation_id, kind)
);

CREATE INDEX eregion_platform_changes_app ON eregion_platform_changes (app_id, created_at DESC);
