-- +goose Up

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
