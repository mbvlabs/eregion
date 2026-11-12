-- One installation has one main-node definition DNS policy. Credentials remain
-- in their existing encrypted provider scope; this row contains metadata only.
CREATE TABLE eregion_definition_dns_policy (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    enabled boolean NOT NULL DEFAULT false,
    provider text,
    zone_id text,
    origins text[] NOT NULL DEFAULT '{}',
    proxy_mode text NOT NULL DEFAULT 'dns-only' CHECK (proxy_mode IN ('dns-only', 'proxied')),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((provider IS NULL AND zone_id IS NULL) OR (provider IS NOT NULL AND provider = 'cloudflare' AND zone_id IS NOT NULL)),
    CHECK (NOT enabled OR (zone_id IS NOT NULL AND cardinality(origins) > 0)),
    FOREIGN KEY (provider, zone_id) REFERENCES eregion_provider_credentials(provider, zone_id)
);
INSERT INTO eregion_definition_dns_policy (singleton) VALUES (true);
