-- Durable storage for the integrations package: private-device access grants
-- and the synchronous integration audit trail. Every control-plane process
-- shares these tables; construction of the stores never runs DDL.

CREATE TABLE eregion_access_grants (
    id text PRIMARY KEY CHECK (id ~ '^grant_[0-9a-f]{32}$'),
    record jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE eregion_integration_audit (
    id bigserial PRIMARY KEY,
    entry jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
