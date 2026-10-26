-- Environment desired state moves from mutable environment columns into numbered,
-- immutable revisions. Each revision records the operation/actor change record
-- and carries a complete source binding, runtime, process formation, plain
-- config and secret names (never secret values). Deployments pin the revision
-- and the exact secret revisions resolved (MASTER D18).

CREATE TABLE eregion_platform_environment_revisions (
    id text PRIMARY KEY CHECK (id ~ '^erv_[0-9a-f]{32}$'),
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    number integer NOT NULL CHECK (number > 0),
    operation_id text REFERENCES eregion_operations(id),
    actor_kind text NOT NULL DEFAULT 'system',
    actor_subject text NOT NULL DEFAULT 'system',
    operation text NOT NULL CHECK (length(operation) BETWEEN 1 AND 64),
    summary text NOT NULL DEFAULT '',
    source jsonb NOT NULL DEFAULT '{}'::jsonb,
    auto_deploy boolean NOT NULL DEFAULT false,
    webhook_secret_name text NOT NULL DEFAULT ''
        CHECK (webhook_secret_name = '' OR webhook_secret_name ~ '^[A-Z][A-Z0-9_]{0,127}$'),
    runtime jsonb NOT NULL DEFAULT '{}'::jsonb,
    processes jsonb NOT NULL DEFAULT '[]'::jsonb,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    secret_names text[] NOT NULL DEFAULT '{}',
    deploy_intent boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (environment_id, number)
);

CREATE INDEX eregion_platform_environment_revisions_environment
    ON eregion_platform_environment_revisions (environment_id, number DESC);

-- Revisions are append-only. Deleting an environment still removes its
-- revisions through the foreign key; a direct UPDATE is refused.
CREATE FUNCTION eregion_platform_environment_revisions_immutable()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'environment revisions are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER eregion_platform_environment_revisions_no_update
    BEFORE UPDATE ON eregion_platform_environment_revisions
    FOR EACH ROW EXECUTE FUNCTION eregion_platform_environment_revisions_immutable();

ALTER TABLE eregion_platform_environments
    ADD COLUMN current_revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL;

-- Existing environments keep working: import one baseline revision from the
-- legacy mutable columns. Its empty source becomes a deployability gap rather
-- than an invented binding.
WITH baseline AS (
    SELECT e.id AS environment_id,
           'erv_' || replace(gen_random_uuid()::text, '-', '') AS revision_id,
           e.env AS config,
           jsonb_build_object(
               'node', e.node_name,
               'health_type', e.health_type,
               'health_path', e.health_path,
               'health_timeout_seconds', e.health_timeout_seconds
           ) AS runtime,
           jsonb_build_array(jsonb_build_object(
               'name', 'web',
               'kind', 'web',
               'command', '',
               'args', '[]'::jsonb,
               'replicas', 1,
               'container_port', e.container_port
           )) AS processes
    FROM eregion_platform_environments e
)
INSERT INTO eregion_platform_environment_revisions
    (id, environment_id, number, operation_id, actor_kind, actor_subject,
     operation, summary, source, auto_deploy, webhook_secret_name, runtime,
     processes, config, secret_names, deploy_intent)
SELECT revision_id, environment_id, 1, NULL, 'system', 'system',
       'legacy.import', 'Imported pre-revision environment desired state.',
       '{}'::jsonb, false, '', runtime, processes, config, '{}', false
FROM baseline;

UPDATE eregion_platform_environments e
SET current_revision_id = r.id
FROM eregion_platform_environment_revisions r
WHERE r.environment_id = e.id AND e.current_revision_id IS NULL;

ALTER TABLE eregion_platform_deployments
    ADD COLUMN revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL,
    ADD COLUMN secret_revisions jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE eregion_platform_domains
    ADD COLUMN is_primary boolean NOT NULL DEFAULT false;

-- Keep one primary domain per environment. Existing bindings choose the
-- earliest one so the partial unique index can be created safely.
UPDATE eregion_platform_domains d
SET is_primary = true
WHERE d.name IN (
    SELECT DISTINCT ON (environment_id) name
    FROM eregion_platform_domains
    ORDER BY environment_id, created_at, name
);

CREATE UNIQUE INDEX eregion_platform_domains_primary
    ON eregion_platform_domains (environment_id) WHERE is_primary;
