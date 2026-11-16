-- name: LockDefinitionPolicyWriters :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: ActiveDefinitionRunExists :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_definition_runs
    WHERE finished_at IS NULL
);

-- name: EnsureDefinitionDNSPolicySingleton :exec
INSERT INTO eregion_definition_dns_policy
    (singleton, revision, enabled, provider, zone_id, origins, proxy_mode, updated_at)
VALUES (true, 0, false, NULL, NULL, sqlc.arg(origins)::text[], sqlc.arg(proxy_mode)::text,
        sqlc.arg(updated_at))
ON CONFLICT (singleton) DO NOTHING;

-- name: GetDefinitionDNSPolicy :one
SELECT p.revision, p.enabled, p.provider, p.zone_id, c.zone_name,
       p.origins, p.proxy_mode
FROM eregion_definition_dns_policy p
LEFT JOIN eregion_provider_credentials c
  ON c.provider = p.provider AND c.zone_id = p.zone_id
WHERE p.singleton = true;

-- name: LockDefinitionDNSPolicy :one
SELECT p.revision, p.enabled, p.provider, p.zone_id, c.zone_name,
       p.origins, p.proxy_mode
FROM eregion_definition_dns_policy p
LEFT JOIN eregion_provider_credentials c
  ON c.provider = p.provider AND c.zone_id = p.zone_id
WHERE p.singleton = true
FOR UPDATE OF p;

-- name: LockDefinitionPolicyProviderZones :many
SELECT zone_id
FROM eregion_provider_credentials
WHERE provider = 'cloudflare'
  AND latest_revision > 0
  AND (zone_id = sqlc.arg(zone_ref)::text OR zone_name = sqlc.arg(zone_ref)::text)
FOR SHARE;

-- name: EnableDefinitionDNSPolicy :exec
UPDATE eregion_definition_dns_policy
SET revision = revision + 1,
    enabled = true,
    provider = 'cloudflare',
    zone_id = sqlc.arg(zone_id)::text,
    origins = sqlc.arg(origins)::text[],
    proxy_mode = sqlc.arg(proxy_mode)::text,
    updated_at = clock_timestamp()
WHERE singleton = true;

-- name: DisableDefinitionDNSPolicy :exec
UPDATE eregion_definition_dns_policy
SET revision = revision + 1,
    enabled = false,
    updated_at = clock_timestamp()
WHERE singleton = true;

-- name: LockDefinitionDNSPolicyShared :one
SELECT p.revision, p.enabled, p.provider, p.zone_id, c.zone_name,
       p.origins, p.proxy_mode
FROM eregion_definition_dns_policy p
LEFT JOIN eregion_provider_credentials c
  ON c.provider = p.provider AND c.zone_id = p.zone_id
WHERE p.singleton = true
FOR SHARE OF p;
