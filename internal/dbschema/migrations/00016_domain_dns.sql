-- +goose Up

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
