-- +goose Up

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
