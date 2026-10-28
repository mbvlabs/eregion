-- D17: a container that could not be removed is recorded and retried by
-- maintenance; it never changes a verified traffic switch.

CREATE TABLE eregion_platform_container_cleanup (
    container_name text PRIMARY KEY
        CHECK (length(container_name) BETWEEN 1 AND 63),
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    node text NOT NULL,
    deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    reason text NOT NULL DEFAULT '',
    attempts integer NOT NULL DEFAULT 1 CHECK (attempts >= 1),
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX eregion_platform_container_cleanup_due
    ON eregion_platform_container_cleanup (next_attempt_at, container_name);
