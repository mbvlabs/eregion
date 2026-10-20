-- Revisioned encrypted secret storage. Applied by the control plane as its next
-- explicit migration (005). Ciphertext is AES-256-GCM under a master key held in
-- a protected file outside PostgreSQL; the database never sees plaintext.
--
-- app/environment match platform environment names. Secret names are valid
-- environment-variable names because resolution is used for worker injection.

CREATE TABLE eregion_secrets (
    app text NOT NULL CHECK (app ~ '^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$'),
    environment text NOT NULL CHECK (environment ~ '^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$'),
    name text NOT NULL CHECK (name ~ '^[A-Z][A-Z0-9_]{0,127}$'),
    latest_revision bigint NOT NULL DEFAULT 0 CHECK (latest_revision >= 0),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','archived')),
    created_by_kind text NOT NULL CHECK (created_by_kind IN ('operator','ci','webhook','system')),
    created_by_subject text NOT NULL CHECK (length(created_by_subject) BETWEEN 1 AND 256),
    updated_by_kind text NOT NULL CHECK (updated_by_kind IN ('operator','ci','webhook','system')),
    updated_by_subject text NOT NULL CHECK (length(updated_by_subject) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    archived_at timestamptz,
    PRIMARY KEY (app, environment, name),
    CHECK ((state = 'archived') = (archived_at IS NOT NULL))
);

CREATE TABLE eregion_secret_revisions (
    app text NOT NULL,
    environment text NOT NULL,
    name text NOT NULL,
    revision bigint NOT NULL CHECK (revision > 0),
    key_id text NOT NULL CHECK (key_id ~ '^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$'),
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 40 AND 40000),
    ciphertext_sha256 text NOT NULL CHECK (ciphertext_sha256 ~ '^[0-9a-f]{64}$'),
    revision_hash text NOT NULL CHECK (revision_hash ~ '^[0-9a-f]{64}$'),
    operation_id text NOT NULL DEFAULT '' CHECK (operation_id = '' OR operation_id ~ '^op_[0-9a-f]{32}$'),
    created_by_kind text NOT NULL CHECK (created_by_kind IN ('operator','ci','webhook','system')),
    created_by_subject text NOT NULL CHECK (length(created_by_subject) BETWEEN 1 AND 256),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (app, environment, name, revision),
    FOREIGN KEY (app, environment, name) REFERENCES eregion_secrets(app, environment, name) ON DELETE CASCADE
);

CREATE INDEX eregion_secrets_environment ON eregion_secrets (app, environment, state, name);
CREATE INDEX eregion_secret_revisions_created ON eregion_secret_revisions (app, environment, name, created_at DESC);
