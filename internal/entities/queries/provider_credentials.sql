-- name: LockProviderCredential :one
SELECT latest_revision, zone_name
FROM eregion_provider_credentials
WHERE provider = sqlc.arg(provider)::text
  AND zone_id = sqlc.arg(zone_id)::text
FOR UPDATE;

-- name: InsertProviderCredential :exec
INSERT INTO eregion_provider_credentials
    (provider, zone_id, zone_name, latest_revision,
     created_by_kind, created_by_subject, updated_by_kind, updated_by_subject,
     created_at, updated_at)
VALUES (sqlc.arg(provider)::text, sqlc.arg(zone_id)::text, sqlc.arg(zone_name)::text, 1,
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        clock_timestamp(), clock_timestamp());

-- name: UpdateProviderCredentialRevision :execrows
UPDATE eregion_provider_credentials
SET latest_revision = sqlc.arg(revision)::bigint,
    updated_by_kind = sqlc.arg(actor_kind)::text,
    updated_by_subject = sqlc.arg(actor_subject)::text,
    updated_at = clock_timestamp()
WHERE provider = sqlc.arg(provider)::text
  AND zone_id = sqlc.arg(zone_id)::text
  AND latest_revision = sqlc.arg(expected_revision)::bigint;

-- name: InsertProviderCredentialRevision :exec
INSERT INTO eregion_provider_credential_revisions
    (provider, zone_id, revision, key_id, ciphertext, ciphertext_sha256,
     revision_hash, created_by_kind, created_by_subject, created_at)
VALUES (sqlc.arg(provider)::text, sqlc.arg(zone_id)::text, sqlc.arg(revision)::bigint,
        sqlc.arg(key_id)::text, sqlc.arg(ciphertext)::bytea, sqlc.arg(ciphertext_sha256)::text,
        sqlc.arg(revision_hash)::text, sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        clock_timestamp());

-- name: GetProviderCredentialMetadata :one
SELECT s.provider, s.zone_id, s.zone_name, s.latest_revision,
       s.created_by_kind, s.created_by_subject,
       s.updated_by_kind, s.updated_by_subject,
       s.created_at, s.updated_at,
       r.revision, r.key_id, r.revision_hash, r.created_at AS revision_created_at
FROM eregion_provider_credentials s
JOIN eregion_provider_credential_revisions r
  ON r.provider = s.provider AND r.zone_id = s.zone_id
 AND r.revision = s.latest_revision
WHERE s.provider = sqlc.arg(provider)::text
  AND s.zone_id = sqlc.arg(zone_id)::text;

-- name: ListProviderCredentialMetadata :many
SELECT s.provider, s.zone_id, s.zone_name, s.latest_revision,
       s.created_by_kind, s.created_by_subject,
       s.updated_by_kind, s.updated_by_subject,
       s.created_at, s.updated_at,
       r.revision, r.key_id, r.revision_hash, r.created_at AS revision_created_at
FROM eregion_provider_credentials s
JOIN eregion_provider_credential_revisions r
  ON r.provider = s.provider AND r.zone_id = s.zone_id
 AND r.revision = s.latest_revision
WHERE s.provider = sqlc.arg(provider)::text
ORDER BY s.zone_name
LIMIT sqlc.arg(max_rows)::integer;

-- name: GetProviderCredentialEncrypted :one
SELECT s.provider, s.zone_id, s.zone_name, s.latest_revision,
       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256,
       r.revision_hash, r.created_at
FROM eregion_provider_credentials s
JOIN eregion_provider_credential_revisions r
  ON r.provider = s.provider AND r.zone_id = s.zone_id
 AND r.revision = s.latest_revision
WHERE s.provider = sqlc.arg(provider)::text
  AND s.zone_id = sqlc.arg(zone_id)::text;

-- name: GetProviderRevisionForZone :one
SELECT latest_revision
FROM eregion_provider_credentials
WHERE provider = sqlc.arg(provider)::text AND zone_id = sqlc.arg(zone_id)::text;

-- name: ListProviderZoneRevisionsForDefinition :many
SELECT zone_id, latest_revision
FROM eregion_provider_credentials
WHERE provider = sqlc.arg(provider)::text
  AND (zone_id = sqlc.arg(zone_id)::text
       OR zone_id IN (
           SELECT x.zone_id
           FROM eregion_platform_domain_dns x
           JOIN eregion_platform_domains d ON d.id = x.domain_id
           WHERE d.environment_id = sqlc.arg(environment_id)::text))
ORDER BY zone_id;

-- name: LockProviderZoneRevisionsForDefinition :many
SELECT zone_id
FROM eregion_provider_credentials
WHERE provider = sqlc.arg(provider)::text
  AND (zone_id = sqlc.arg(zone_id)::text
       OR zone_id IN (
           SELECT x.zone_id
           FROM eregion_platform_domain_dns x
           JOIN eregion_platform_domains d ON d.id = x.domain_id
           WHERE d.environment_id = sqlc.arg(environment_id)::text))
ORDER BY zone_id
FOR SHARE;

-- name: LockProviderRevisionForZoneShare :one
SELECT latest_revision
FROM eregion_provider_credentials
WHERE provider = sqlc.arg(provider)::text AND zone_id = sqlc.arg(zone_id)::text
FOR SHARE;
