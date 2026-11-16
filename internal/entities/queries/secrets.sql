-- Secret persistence for the vault's typed ports. These queries preserve the
-- existing statements in internal/vault/store.go: the same row locks, CAS
-- guards, revision pointers and ciphertext columns. They are foundation only
-- until the coordinated cutover wires the vault to entities.

-- name: LockSecretLatestRevision :one
SELECT latest_revision
FROM eregion_secrets
WHERE app = sqlc.arg(app)::text
  AND environment = sqlc.arg(environment)::text
  AND name = sqlc.arg(name)::text
FOR UPDATE;

-- name: LockSecretNamespace :one
SELECT latest_revision, state
FROM eregion_secrets
WHERE app = sqlc.arg(app)::text
  AND environment = sqlc.arg(environment)::text
  AND name = sqlc.arg(name)::text
FOR UPDATE;

-- name: LockSecretMetadataRow :one
SELECT name
FROM eregion_secrets
WHERE app = sqlc.arg(app)::text
  AND environment = sqlc.arg(environment)::text
  AND name = sqlc.arg(name)::text
FOR UPDATE;

-- name: InsertSecretNamespace :exec
INSERT INTO eregion_secrets
    (app, environment, name, latest_revision, state,
     created_by_kind, created_by_subject, updated_by_kind, updated_by_subject,
     created_at, updated_at)
VALUES (sqlc.arg(app)::text, sqlc.arg(environment)::text, sqlc.arg(name)::text, 1, 'active',
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        clock_timestamp(), clock_timestamp());

-- name: AdvanceSecretNamespace :execrows
UPDATE eregion_secrets
SET latest_revision = sqlc.arg(revision)::bigint,
    state = 'active',
    archived_at = NULL,
    updated_by_kind = sqlc.arg(actor_kind)::text,
    updated_by_subject = sqlc.arg(actor_subject)::text,
    updated_at = clock_timestamp()
WHERE app = sqlc.arg(app)::text
  AND environment = sqlc.arg(environment)::text
  AND name = sqlc.arg(name)::text
  AND latest_revision = sqlc.arg(expected_revision)::bigint;

-- name: ArchiveSecretNamespace :execrows
UPDATE eregion_secrets
SET state = 'archived',
    archived_at = clock_timestamp(),
    updated_by_kind = sqlc.arg(actor_kind)::text,
    updated_by_subject = sqlc.arg(actor_subject)::text,
    updated_at = clock_timestamp()
WHERE app = sqlc.arg(app)::text
  AND environment = sqlc.arg(environment)::text
  AND name = sqlc.arg(name)::text
  AND latest_revision = sqlc.arg(expected_revision)::bigint;

-- name: InsertSecretRevision :exec
INSERT INTO eregion_secret_revisions
    (app, environment, name, revision, key_id, ciphertext, ciphertext_sha256,
     revision_hash, created_by_kind, created_by_subject, operation_id, created_at)
VALUES (sqlc.arg(app)::text, sqlc.arg(environment)::text, sqlc.arg(name)::text,
        sqlc.arg(revision)::bigint, sqlc.arg(key_id)::text, sqlc.arg(ciphertext)::bytea,
        sqlc.arg(ciphertext_sha256)::text, sqlc.arg(revision_hash)::text,
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text, '', clock_timestamp());

-- name: GetSecretMetadata :one
SELECT s.app, s.environment, s.name, s.latest_revision, s.state,
       s.created_by_kind, s.created_by_subject,
       s.updated_by_kind, s.updated_by_subject,
       s.created_at, s.updated_at, s.archived_at,
       r.revision, r.key_id, r.revision_hash, r.created_at AS revision_created_at
FROM eregion_secrets s
JOIN eregion_secret_revisions r
  ON r.app = s.app AND r.environment = s.environment
 AND r.name = s.name AND r.revision = s.latest_revision
WHERE s.app = sqlc.arg(app)::text
  AND s.environment = sqlc.arg(environment)::text
  AND s.name = sqlc.arg(name)::text;

-- name: ListSecretMetadata :many
SELECT s.app, s.environment, s.name, s.latest_revision, s.state,
       s.created_by_kind, s.created_by_subject,
       s.updated_by_kind, s.updated_by_subject,
       s.created_at, s.updated_at, s.archived_at,
       r.revision, r.key_id, r.revision_hash, r.created_at AS revision_created_at
FROM eregion_secrets s
JOIN eregion_secret_revisions r
  ON r.app = s.app AND r.environment = s.environment
 AND r.name = s.name AND r.revision = s.latest_revision
WHERE s.app = sqlc.arg(app)::text
  AND s.environment = sqlc.arg(environment)::text
ORDER BY s.name
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListActiveSecretCiphertexts :many
SELECT s.app, s.environment, s.name, s.state,
       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256,
       r.revision_hash, r.created_at
FROM eregion_secrets s
JOIN eregion_secret_revisions r
  ON r.app = s.app AND r.environment = s.environment
 AND r.name = s.name AND r.revision = s.latest_revision
WHERE s.app = sqlc.arg(app)::text
  AND s.environment = sqlc.arg(environment)::text
  AND s.state = 'active'
ORDER BY s.name
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListPinnedSecretCiphertexts :many
SELECT s.app, s.environment, s.name, s.state,
       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256,
       r.revision_hash, r.created_at
FROM eregion_secrets s
JOIN eregion_secret_revisions r
  ON r.app = s.app AND r.environment = s.environment
 AND r.name = s.name
JOIN jsonb_to_recordset(sqlc.arg(pins)::jsonb) AS want(name text, revision bigint)
  ON want.name = s.name AND want.revision = r.revision
WHERE s.app = sqlc.arg(app)::text
  AND s.environment = sqlc.arg(environment)::text
ORDER BY s.name;

-- name: GetLatestSecretCiphertext :one
SELECT s.app, s.environment, s.name, s.state,
       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256,
       r.revision_hash, r.created_at
FROM eregion_secrets s
JOIN eregion_secret_revisions r
  ON r.app = s.app AND r.environment = s.environment
 AND r.name = s.name AND r.revision = s.latest_revision
WHERE s.app = sqlc.arg(app)::text
  AND s.environment = sqlc.arg(environment)::text
  AND s.name = sqlc.arg(name)::text;

-- name: ListDefinitionSecrets :many
SELECT name, latest_revision, state
FROM eregion_secrets
WHERE app = sqlc.arg(app)::text AND environment = sqlc.arg(environment)::text
ORDER BY name;

-- name: LockDefinitionSecretNames :many
SELECT name
FROM eregion_secrets
WHERE app = sqlc.arg(app)::text AND environment = sqlc.arg(environment)::text
ORDER BY name
FOR SHARE;

-- name: LockSecretBinding :one
SELECT 1
FROM eregion_secrets
WHERE app = sqlc.arg(app)::text AND environment = sqlc.arg(environment)::text
  AND name = sqlc.arg(name)::text
FOR SHARE;

-- name: DeleteSecretsForEnvironment :execrows
DELETE FROM eregion_secrets
WHERE app = sqlc.arg(app)::text AND environment = sqlc.arg(environment)::text;

-- name: DeleteSecretsForApp :execrows
DELETE FROM eregion_secrets WHERE app = sqlc.arg(app)::text;
