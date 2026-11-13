-- +goose Up
-- Fresh installations only. Application code supplies values and validates
-- domain rules; cyclic environment/release backpointers are not foreign keys.

CREATE TABLE eregion_operations (
    id text PRIMARY KEY,
    kind text NOT NULL,
    target text NOT NULL,
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    idempotency_key text NOT NULL,
    input_hash text NOT NULL,
    input jsonb NOT NULL,
    state text NOT NULL,
    attempt integer NOT NULL,
    completed_steps integer NOT NULL,
    last_sequence bigint NOT NULL,
    job_id bigint UNIQUE,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    error_code text NOT NULL,
    UNIQUE (actor_kind, actor_subject, kind, target, idempotency_key)
);
CREATE TABLE eregion_operation_events (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    sequence bigint NOT NULL,
    event jsonb NOT NULL,
    PRIMARY KEY (operation_id, sequence)
);
CREATE TABLE eregion_operation_steps (
    operation_id text NOT NULL REFERENCES eregion_operations(id),
    step text NOT NULL,
    inputs_hash text NOT NULL,
    state text NOT NULL,
    attempt integer NOT NULL,
    execution_id text NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    error text NOT NULL,
    result jsonb,
    error_code text NOT NULL,
    PRIMARY KEY (operation_id, step)
);
CREATE INDEX eregion_operations_unfinished ON eregion_operations (job_id)
    WHERE state IN ('queued','running','cancelling');
CREATE INDEX eregion_operation_steps_incomplete ON eregion_operation_steps (operation_id)
    WHERE state <> 'completed';

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
CREATE TABLE eregion_platform_environment_revisions (
    id text PRIMARY KEY,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    number integer NOT NULL,
    operation_id text REFERENCES eregion_operations(id),
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    operation text NOT NULL,
    summary text NOT NULL,
    source jsonb NOT NULL,
    auto_deploy boolean NOT NULL,
    webhook_secret_name text NOT NULL,
    runtime jsonb NOT NULL,
    processes jsonb NOT NULL,
    config jsonb NOT NULL,
    secret_names text[] NOT NULL,
    deploy_intent boolean NOT NULL,
    created_at timestamptz NOT NULL,
    retention jsonb NOT NULL,
    resources text[] NOT NULL,
    UNIQUE (environment_id, number)
);
CREATE INDEX eregion_platform_environment_revisions_environment ON eregion_platform_environment_revisions (environment_id, number DESC);
CREATE TABLE eregion_platform_releases (
    id text PRIMARY KEY,
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    image text NOT NULL,
    image_id text NOT NULL,
    source jsonb NOT NULL,
    build jsonb,
    command jsonb,
    status text NOT NULL,
    error_code text NOT NULL,
    error text NOT NULL,
    created_at timestamptz NOT NULL,
    completed_at timestamptz,
    build_id text,
    commit_sha text NOT NULL,
    pruned_at timestamptz
);
CREATE INDEX eregion_platform_releases_environment ON eregion_platform_releases (environment_id, created_at DESC);
CREATE TABLE eregion_platform_builds (
    id text PRIMARY KEY,
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL,
    release_id text REFERENCES eregion_platform_releases(id) ON DELETE SET NULL,
    source jsonb NOT NULL,
    commit_sha text NOT NULL,
    state text NOT NULL,
    error_code text NOT NULL,
    error text NOT NULL,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz
);
CREATE INDEX eregion_platform_builds_environment ON eregion_platform_builds (environment_id, created_at DESC);
CREATE TABLE eregion_platform_deployments (
    id text PRIMARY KEY,
    operation_id text NOT NULL UNIQUE REFERENCES eregion_operations(id),
    app_id text NOT NULL REFERENCES eregion_platform_apps(id) ON DELETE CASCADE,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    action text NOT NULL,
    release_id text NOT NULL REFERENCES eregion_platform_releases(id),
    previous_release_id text REFERENCES eregion_platform_releases(id),
    state text NOT NULL,
    error_code text NOT NULL,
    message text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    revision_id text REFERENCES eregion_platform_environment_revisions(id) ON DELETE SET NULL,
    secret_revisions jsonb NOT NULL,
    previous_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    instances jsonb NOT NULL
);
CREATE INDEX eregion_platform_deployments_environment ON eregion_platform_deployments (environment_id, created_at DESC);
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

CREATE TABLE eregion_platform_domains (
    name text PRIMARY KEY,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    is_primary boolean NOT NULL,
    desired_revision bigint NOT NULL,
    applied_revision bigint NOT NULL,
    route_state text NOT NULL,
    route_error text NOT NULL,
    route_applied_at timestamptz,
    route_observed_at timestamptz,
    expected_addresses text[] NOT NULL,
    dns_state text NOT NULL,
    dns_addresses text[] NOT NULL,
    dns_error text NOT NULL,
    dns_observed_at timestamptz,
    certificate_state text NOT NULL,
    certificate_not_after timestamptz,
    certificate_error text NOT NULL,
    certificate_observed_at timestamptz,
    public_state text NOT NULL,
    public_status integer,
    public_error text NOT NULL,
    public_observed_at timestamptz,
    observed_at timestamptz,
    check_path text NOT NULL,
    id text NOT NULL
);
CREATE INDEX eregion_platform_domains_environment ON eregion_platform_domains(environment_id);
CREATE UNIQUE INDEX eregion_platform_domains_primary ON eregion_platform_domains (environment_id) WHERE is_primary;
CREATE INDEX eregion_platform_domains_route_pending ON eregion_platform_domains (environment_id, name) WHERE route_state <> 'applied' OR applied_revision < desired_revision;
CREATE UNIQUE INDEX eregion_platform_domains_id_key ON eregion_platform_domains(id);
CREATE TABLE eregion_platform_domain_dns (
    domain_id text PRIMARY KEY REFERENCES eregion_platform_domains(id) ON DELETE RESTRICT,
    provider text NOT NULL,
    zone_id text NOT NULL,
    zone_name text NOT NULL,
    ownership_marker text NOT NULL UNIQUE,
    generation bigint NOT NULL,
    desired_origins text[] NOT NULL,
    desired_proxied boolean NOT NULL,
    state text NOT NULL,
    last_operation_id text NOT NULL,
    last_error text NOT NULL,
    applied_generation bigint NOT NULL,
    cleanup_error text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    applied_at timestamptz,
    cleanup_requested_at timestamptz,
    cleanup_verified_at timestamptz
);
CREATE INDEX eregion_platform_domain_dns_state ON eregion_platform_domain_dns(state);
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
CREATE TABLE eregion_platform_container_cleanup (
    container_name text PRIMARY KEY,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    node text NOT NULL,
    deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    reason text NOT NULL,
    attempts integer NOT NULL,
    last_error text NOT NULL,
    created_at timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL
);
CREATE INDEX eregion_platform_container_cleanup_due ON eregion_platform_container_cleanup (next_attempt_at, container_name);
CREATE TABLE eregion_platform_deployment_owners (
    environment_id text PRIMARY KEY REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    operation_id text REFERENCES eregion_operations(id) ON DELETE CASCADE,
    deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE CASCADE,
    owner_attempt integer,
    phase text NOT NULL,
    route_target_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    restore_release_id text REFERENCES eregion_platform_releases(id),
    restore_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX eregion_platform_deployment_owners_deployment ON eregion_platform_deployment_owners (deployment_id);
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

CREATE TABLE eregion_secrets (
    app text NOT NULL,
    environment text NOT NULL,
    name text NOT NULL,
    latest_revision bigint NOT NULL,
    state text NOT NULL,
    created_by_kind text NOT NULL,
    created_by_subject text NOT NULL,
    updated_by_kind text NOT NULL,
    updated_by_subject text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    PRIMARY KEY (app, environment, name)
);
CREATE TABLE eregion_secret_revisions (
    app text NOT NULL,
    environment text NOT NULL,
    name text NOT NULL,
    revision bigint NOT NULL,
    key_id text NOT NULL,
    ciphertext bytea NOT NULL,
    ciphertext_sha256 text NOT NULL,
    revision_hash text NOT NULL,
    operation_id text NOT NULL,
    created_by_kind text NOT NULL,
    created_by_subject text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (app, environment, name, revision),
    FOREIGN KEY (app, environment, name) REFERENCES eregion_secrets(app, environment, name) ON DELETE CASCADE
);
CREATE INDEX eregion_secrets_environment ON eregion_secrets (app, environment, state, name);
CREATE INDEX eregion_secret_revisions_created ON eregion_secret_revisions (app, environment, name, created_at DESC);
CREATE TABLE eregion_provider_credentials (
    provider text NOT NULL,
    zone_id text NOT NULL,
    zone_name text NOT NULL,
    latest_revision bigint NOT NULL,
    created_by_kind text NOT NULL,
    created_by_subject text NOT NULL,
    updated_by_kind text NOT NULL,
    updated_by_subject text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (provider, zone_id),
    UNIQUE (provider, zone_name)
);
CREATE TABLE eregion_provider_credential_revisions (
    provider text NOT NULL,
    zone_id text NOT NULL,
    revision bigint NOT NULL,
    key_id text NOT NULL,
    ciphertext bytea NOT NULL,
    ciphertext_sha256 text NOT NULL,
    revision_hash text NOT NULL,
    created_by_kind text NOT NULL,
    created_by_subject text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (provider, zone_id, revision),
    FOREIGN KEY (provider, zone_id) REFERENCES eregion_provider_credentials(provider, zone_id) ON DELETE CASCADE
);
CREATE INDEX eregion_provider_credential_revisions_created ON eregion_provider_credential_revisions (provider, zone_id, created_at DESC);
CREATE TABLE eregion_access_grants (
    id text PRIMARY KEY,
    record jsonb NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE TABLE eregion_integration_audit (
    id bigint PRIMARY KEY,
    entry jsonb NOT NULL,
    recorded_at timestamptz NOT NULL
);
CREATE TABLE eregion_webhook_deliveries (
    delivery_id_hash text NOT NULL,
    body_sha256 text NOT NULL,
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    kind text NOT NULL,
    target text NOT NULL,
    state text NOT NULL,
    operation_id text,
    first_seen_at timestamptz NOT NULL,
    submitted_at timestamptz,
    environment_id text NOT NULL,
    revision_id text NOT NULL,
    secret_revision bigint NOT NULL,
    secret_revision_hash text NOT NULL,
    repository text NOT NULL,
    branch text NOT NULL,
    commit_sha text NOT NULL,
    job_id bigint,
    admitted_at timestamptz,
    failed_at timestamptz,
    error_code text NOT NULL,
    error_detail text NOT NULL,
    admission_attempts integer NOT NULL,
    PRIMARY KEY (environment_id, delivery_id_hash)
);
CREATE INDEX eregion_webhook_deliveries_first_seen ON eregion_webhook_deliveries (first_seen_at);
CREATE INDEX eregion_webhook_deliveries_admitted ON eregion_webhook_deliveries (job_id) WHERE state = 'admitted';
CREATE TABLE eregion_server_settings (
    singleton boolean PRIMARY KEY,
    retention jsonb NOT NULL,
    updated_at timestamptz NOT NULL,
    retention_gc_pending boolean NOT NULL
);
CREATE TABLE eregion_retention_runs (
    id text PRIMARY KEY,
    operation_id text REFERENCES eregion_operations(id),
    status text NOT NULL,
    planned_items jsonb NOT NULL,
    removed_items jsonb NOT NULL,
    planned_bytes bigint NOT NULL,
    reclaimed_bytes bigint NOT NULL,
    errors jsonb NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    cache_bytes bigint,
    cache_measured_at timestamptz,
    cache_measurement_error text NOT NULL,
    gc_generation integer NOT NULL
);
CREATE INDEX eregion_retention_runs_started ON eregion_retention_runs (started_at DESC);
CREATE TABLE eregion_workspace (
    singleton boolean PRIMARY KEY,
    workspace_id text NOT NULL UNIQUE,
    name text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL,
    registry_host_node text,
    registry_host_key_type text,
    registry_host_key_fingerprint text,
    registry_host_state_dir text
);
CREATE TABLE eregion_definition_dns_policy (
    singleton boolean PRIMARY KEY,
    revision bigint NOT NULL,
    enabled boolean NOT NULL,
    provider text,
    zone_id text,
    origins text[] NOT NULL,
    proxy_mode text NOT NULL,
    updated_at timestamptz NOT NULL,
    FOREIGN KEY (provider, zone_id) REFERENCES eregion_provider_credentials(provider, zone_id)
);
CREATE TABLE eregion_definition_runs (
    operation_id text PRIMARY KEY REFERENCES eregion_operations(id),
    plan_nonce text NOT NULL UNIQUE,
    environment_id text NOT NULL,
    approved jsonb NOT NULL,
    next_step integer NOT NULL,
    failure_code text NOT NULL,
    failure_message text NOT NULL,
    finished_at timestamptz
);
CREATE UNIQUE INDEX eregion_definition_runs_active_environment ON eregion_definition_runs(environment_id) WHERE finished_at IS NULL;
CREATE TABLE eregion_definition_steps (
    parent_operation_id text NOT NULL REFERENCES eregion_definition_runs(operation_id),
    ordinal integer NOT NULL,
    kind text NOT NULL,
    target text NOT NULL,
    input jsonb NOT NULL,
    domain_id text,
    allocated_domain_id text UNIQUE,
    dns_plan jsonb,
    dns_cleanup jsonb,
    child_operation_id text UNIQUE REFERENCES eregion_operations(id) DEFERRABLE INITIALLY DEFERRED,
    dns_progress jsonb NOT NULL,
    PRIMARY KEY (parent_operation_id, ordinal)
);
