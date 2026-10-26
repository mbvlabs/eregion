-- A domain binding is desired state. The Caddy route, the DNS record, the
-- certificate and the public response are separate applied/observed facts.
-- Route writes advance desired_revision, one successful reload marks the
-- applied revision, and the observation job records DNS, TLS and HTTP results
-- without changing the binding.

ALTER TABLE eregion_platform_domains
    ADD COLUMN desired_revision bigint NOT NULL DEFAULT 1 CHECK (desired_revision >= 1),
    ADD COLUMN applied_revision bigint NOT NULL DEFAULT 0 CHECK (applied_revision >= 0),
    ADD COLUMN route_state text NOT NULL DEFAULT 'pending'
        CHECK (route_state IN ('pending','applied','failed')),
    ADD COLUMN route_error text NOT NULL DEFAULT '',
    ADD COLUMN route_applied_at timestamptz,
    ADD COLUMN route_observed_at timestamptz,
    ADD COLUMN expected_addresses text[] NOT NULL DEFAULT '{}',
    ADD COLUMN dns_state text NOT NULL DEFAULT 'pending'
        CHECK (dns_state IN ('pending','ok','wrong','proxied')),
    ADD COLUMN dns_addresses text[] NOT NULL DEFAULT '{}',
    ADD COLUMN dns_error text NOT NULL DEFAULT '',
    ADD COLUMN dns_observed_at timestamptz,
    ADD COLUMN certificate_state text NOT NULL DEFAULT 'pending'
        CHECK (certificate_state IN ('pending','valid','failed')),
    ADD COLUMN certificate_not_after timestamptz,
    ADD COLUMN certificate_error text NOT NULL DEFAULT '',
    ADD COLUMN certificate_observed_at timestamptz,
    ADD COLUMN public_state text NOT NULL DEFAULT 'pending'
        CHECK (public_state IN ('pending','ok','failed')),
    ADD COLUMN public_status integer
        CHECK (public_status IS NULL OR public_status BETWEEN 100 AND 599),
    ADD COLUMN public_error text NOT NULL DEFAULT '',
    ADD COLUMN public_observed_at timestamptz,
    ADD COLUMN observed_at timestamptz;

CREATE INDEX eregion_platform_domains_route_pending
    ON eregion_platform_domains (environment_id, name)
    WHERE route_state <> 'applied' OR applied_revision < desired_revision;
