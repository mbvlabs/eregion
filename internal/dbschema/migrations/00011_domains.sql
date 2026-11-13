-- +goose Up

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
