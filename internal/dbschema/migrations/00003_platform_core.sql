-- +goose Up

CREATE TABLE eregion_platform_nodes (
    name text PRIMARY KEY,
    roles text[] NOT NULL,
    capabilities text[] NOT NULL,
    address text NOT NULL,
    status text NOT NULL,
    last_seen_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    enrollment_state text NOT NULL,
    enrollment_error text NOT NULL,
    enroll_operation_id text,
    enrolled_at timestamptz,
    transport text NOT NULL,
    ssh_address text NOT NULL,
    ssh_user text NOT NULL,
    ssh_host_key text NOT NULL,
    ssh_host_key_fingerprint text NOT NULL,
    expected_identity_fingerprint text NOT NULL,
    identity_public_key text NOT NULL,
    identity_fingerprint text NOT NULL,
    os_id text NOT NULL,
    os_version text NOT NULL,
    arch text NOT NULL,
    probe jsonb,
    build_concurrency integer NOT NULL,
    build_user text NOT NULL,
    wg_enabled boolean NOT NULL,
    wg_address text NOT NULL,
    wg_endpoint text NOT NULL,
    wg_ssh_port integer NOT NULL,
    wg_public_key text NOT NULL,
    control_wg_public_key text NOT NULL,
    wg_state text NOT NULL,
    wg_reconciled_at timestamptz,
    last_check_at timestamptz,
    last_check_state text NOT NULL,
    last_check_detail text NOT NULL
);

CREATE UNIQUE INDEX eregion_platform_nodes_wg_address ON eregion_platform_nodes(wg_address) WHERE wg_address <> '';

CREATE UNIQUE INDEX eregion_platform_nodes_identity_key ON eregion_platform_nodes(identity_public_key) WHERE identity_public_key <> '';

CREATE TABLE eregion_platform_apps (
    id text PRIMARY KEY,
    name text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    image_repository text NOT NULL
);

CREATE UNIQUE INDEX eregion_platform_apps_image_repository_unique ON eregion_platform_apps (image_repository) WHERE image_repository <> '';

CREATE TABLE eregion_platform_environments (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    name text NOT NULL,
    node_name text NOT NULL REFERENCES eregion_platform_nodes(name),
    host_port integer NOT NULL UNIQUE,
    candidate_port integer NOT NULL UNIQUE,
    container_port integer NOT NULL,
    bind_address text NOT NULL,
    health_type text NOT NULL,
    health_path text NOT NULL,
    health_timeout_seconds integer NOT NULL,
    env jsonb NOT NULL,
    secret_refs jsonb NOT NULL,
    secret_revision bigint NOT NULL,
    desired_release_id text,
    applied_release_id text,
    serving_release_id text,
    health_state text NOT NULL,
    health_detail text NOT NULL,
    health_checked_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    build_node text REFERENCES eregion_platform_nodes(name),
    current_revision_id text,
    serving_deployment_id text,
    protected boolean NOT NULL,
    UNIQUE (app_id, name)
);

CREATE TABLE eregion_platform_changes (
    id text PRIMARY KEY,
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    sequence bigint NOT NULL,
    kind text NOT NULL,
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    summary text NOT NULL,
    desired jsonb,
    applied jsonb,
    created_at timestamptz NOT NULL,
    UNIQUE (environment_id, sequence),
    UNIQUE (operation_id, kind)
);

CREATE INDEX eregion_platform_changes_app ON eregion_platform_changes (app_id, created_at DESC);
