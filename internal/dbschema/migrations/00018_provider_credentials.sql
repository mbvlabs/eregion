-- +goose Up

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
