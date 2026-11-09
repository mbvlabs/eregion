-- Provider credentials are a typed scope separate from application secrets.
-- They are keyed by provider and provider zone ID, never by an app/environment
-- secret namespace, and no generic secret list/export/archive path can address
-- them. Ciphertext is AES-256-GCM under the same protected master-key ring;
-- the authenticated data binds provider, zone ID and revision so a revision
-- cannot be moved between scopes.

CREATE TABLE eregion_provider_credentials (
    provider text NOT NULL CHECK (provider IN ('cloudflare')),
    zone_id text NOT NULL CHECK (zone_id ~ '^[0-9a-f]{32}$'),
    zone_name text NOT NULL CHECK (
        zone_name ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])$'
        AND position('.' in zone_name) > 1
        AND position('..' in zone_name) = 0
    ),
    latest_revision bigint NOT NULL DEFAULT 0 CHECK (latest_revision >= 0),
    created_by_kind text NOT NULL CHECK (created_by_kind IN ('operator','ci','webhook','system')),
    created_by_subject text NOT NULL CHECK (length(created_by_subject) BETWEEN 1 AND 256),
    updated_by_kind text NOT NULL CHECK (updated_by_kind IN ('operator','ci','webhook','system')),
    updated_by_subject text NOT NULL CHECK (length(updated_by_subject) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (provider, zone_id),
    UNIQUE (provider, zone_name)
);

CREATE TABLE eregion_provider_credential_revisions (
    provider text NOT NULL,
    zone_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    key_id text NOT NULL CHECK (key_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$'),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 40 AND 40000),
    ciphertext_sha256 text NOT NULL CHECK (ciphertext_sha256 ~ '^[0-9a-f]{64}$'),
    revision_hash text NOT NULL CHECK (revision_hash ~ '^[0-9a-f]{64}$'),
    created_by_kind text NOT NULL CHECK (created_by_kind IN ('operator','ci','webhook','system')),
    created_by_subject text NOT NULL CHECK (length(created_by_subject) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (provider, zone_id, revision),
    FOREIGN KEY (provider, zone_id) REFERENCES eregion_provider_credentials(provider, zone_id) ON DELETE CASCADE
);

CREATE INDEX eregion_provider_credential_revisions_created
    ON eregion_provider_credential_revisions (provider, zone_id, created_at DESC);
