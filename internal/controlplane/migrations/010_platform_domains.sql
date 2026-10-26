-- Public hostnames that Caddy on the main node routes to an environment's
-- stable host port. A deploy never changes the route: the serving container
-- always listens on host_port.
CREATE TABLE eregion_platform_domains (
    name text PRIMARY KEY CHECK (name ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$'),
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX eregion_platform_domains_environment ON eregion_platform_domains(environment_id);
