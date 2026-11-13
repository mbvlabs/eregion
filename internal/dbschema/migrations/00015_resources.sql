-- +goose Up

CREATE TABLE eregion_platform_resources (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    name text NOT NULL,
    node_name text NOT NULL REFERENCES eregion_platform_nodes(name),
    spec jsonb NOT NULL,
    state text NOT NULL,
    image text NOT NULL,
    image_id text NOT NULL,
    image_digest text NOT NULL,
    container_name text NOT NULL,
    network_name text NOT NULL,
    volumes jsonb NOT NULL,
    health_state text NOT NULL,
    health_detail text NOT NULL,
    health_checked_at timestamptz,
    operation_id text REFERENCES eregion_operations(id),
    error_code text NOT NULL,
    error text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    destroyed_at timestamptz,
    UNIQUE (environment_id, name)
);

CREATE INDEX eregion_platform_resources_environment ON eregion_platform_resources (environment_id, name);

CREATE INDEX eregion_platform_resources_app ON eregion_platform_resources (app_id, environment_id, name);
