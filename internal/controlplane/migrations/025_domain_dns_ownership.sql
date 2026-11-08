-- Durable managed-DNS ownership. The domain binding gets an immutable ID so a
-- recreated hostname cannot inherit a previous incarnation's provider record.
ALTER TABLE eregion_platform_domains ADD COLUMN id text;
UPDATE eregion_platform_domains
SET id = 'dom_' || replace(gen_random_uuid()::text, '-', '')
WHERE id IS NULL;
ALTER TABLE eregion_platform_domains ALTER COLUMN id SET NOT NULL;
ALTER TABLE eregion_platform_domains ALTER COLUMN id SET DEFAULT 'dom_' || replace(gen_random_uuid()::text, '-', '');
CREATE UNIQUE INDEX eregion_platform_domains_id_key ON eregion_platform_domains(id);

-- One row per managed binding. It is the durable desired intent and cleanup
-- ledger, written in the same transaction as the operation and River job. The
-- row is retained until provider cleanup is verified; the domain FK restricts
-- deletion so a binding cannot disappear while an owned record may remain.
CREATE TABLE eregion_platform_domain_dns (
    domain_id text PRIMARY KEY REFERENCES eregion_platform_domains(id) ON DELETE RESTRICT,
    provider text NOT NULL CHECK (provider IN ('cloudflare')),
    zone_id text NOT NULL CHECK (zone_id ~ '^[0-9a-f]{32}$'),
    zone_name text NOT NULL CHECK (length(zone_name) > 0),
    ownership_marker text NOT NULL UNIQUE CHECK (length(ownership_marker) BETWEEN 1 AND 256),
    generation bigint NOT NULL DEFAULT 1 CHECK (generation >= 1),
    desired_origins text[] NOT NULL,
    desired_proxied boolean NOT NULL,
    state text NOT NULL CHECK (state IN ('pending', 'applied', 'failed', 'cleanup_pending', 'cleanup_verified', 'cleanup_failed')),
    last_operation_id text NOT NULL,
    last_error text NOT NULL DEFAULT '',
    applied_generation bigint NOT NULL DEFAULT 0 CHECK (applied_generation >= 0),
    cleanup_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    applied_at timestamptz,
    cleanup_requested_at timestamptz,
    cleanup_verified_at timestamptz,
    CHECK (length(last_error) <= 1000),
    CHECK (length(cleanup_error) <= 1000),
    CHECK (cardinality(desired_origins) BETWEEN 0 AND 8),
    CHECK (
        (state IN ('cleanup_pending', 'cleanup_verified', 'cleanup_failed') AND cleanup_requested_at IS NOT NULL)
        OR state NOT IN ('cleanup_pending', 'cleanup_verified', 'cleanup_failed')
    )
);
CREATE INDEX eregion_platform_domain_dns_state ON eregion_platform_domain_dns(state);
