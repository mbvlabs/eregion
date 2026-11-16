-- Platform domain, domain-DNS ledger, deployment-owner, cleanup and release
-- command persistence for the entities foundation.

-- ---------------------------------------------------------------- domains

-- name: GetPlatformDomainByName :one
SELECT id, environment_id
FROM eregion_platform_domains
WHERE name=sqlc.arg(name)::text;

-- name: LockPlatformDomainByNameAndEnvironment :one
SELECT id
FROM eregion_platform_domains
WHERE name=sqlc.arg(name)::text AND environment_id=sqlc.arg(environment_id)::text
FOR UPDATE;

-- name: GetPlatformDomainBindingByNameAndEnvironment :one
SELECT id,is_primary
FROM eregion_platform_domains
WHERE name=sqlc.arg(name)::text AND environment_id=sqlc.arg(environment_id)::text;

-- name: GetPlatformDomainEnvironmentIDByName :one
SELECT environment_id
FROM eregion_platform_domains
WHERE name=sqlc.arg(name)::text;

-- name: GetPlatformDomainEnvironmentIDByID :one
SELECT environment_id
FROM eregion_platform_domains
WHERE id=sqlc.arg(id)::text;

-- name: LockPlatformDomainByIDAndEnvironment :one
SELECT id
FROM eregion_platform_domains
WHERE id=sqlc.arg(id)::text AND environment_id=sqlc.arg(environment_id)::text
FOR UPDATE;

-- name: PlatformDomainExistsByID :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_domains WHERE id=sqlc.arg(id)::text);

-- name: PlatformDomainBindingExists :one
SELECT EXISTS(
  SELECT 1 FROM eregion_platform_domains d
  JOIN eregion_platform_environments e ON e.id=d.environment_id
  JOIN eregion_platform_apps a ON a.id=e.app_id
  WHERE d.name=sqlc.arg(domain)::text AND a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text);

-- name: PlatformDomainHasPrimary :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_domains WHERE environment_id=sqlc.arg(environment_id)::text AND is_primary);

-- name: GetPlatformPrimaryDNSState :one
SELECT dns_state
FROM eregion_platform_domains
WHERE environment_id=sqlc.arg(environment_id)::text AND is_primary
LIMIT 1;

-- name: GetPlatformPrimaryCheckPath :one
SELECT coalesce(d.check_path,'')
FROM eregion_platform_domains d
WHERE d.environment_id=sqlc.arg(environment_id)::text AND d.name=sqlc.arg(name)::text AND d.is_primary
LIMIT 1;

-- name: SetPlatformDomainExpectedAddressesByID :execrows
UPDATE eregion_platform_domains
SET expected_addresses=sqlc.arg(addresses)::text[]
WHERE id=sqlc.arg(id)::text;

-- name: SetPlatformDomainExpectedAddressesByName :execrows
UPDATE eregion_platform_domains
SET expected_addresses=sqlc.arg(addresses)::text[]
WHERE name=sqlc.arg(name)::text;

-- name: SetPlatformDomainCheckPath :execrows
UPDATE eregion_platform_domains
SET check_path=sqlc.arg(check_path)::text
WHERE name=sqlc.arg(name)::text;

-- name: ClearPlatformPrimaryExcept :execrows
UPDATE eregion_platform_domains
SET is_primary=false
WHERE environment_id=sqlc.arg(environment_id)::text AND is_primary AND name<>sqlc.arg(name)::text;

-- name: PromotePlatformDomainPrimary :execrows
UPDATE eregion_platform_domains
SET is_primary=true, desired_revision=desired_revision+1
WHERE name=sqlc.arg(name)::text;

-- name: ClearPlatformPrimary :execrows
UPDATE eregion_platform_domains
SET is_primary=false
WHERE environment_id=sqlc.arg(environment_id)::text AND is_primary;

-- name: InsertPlatformDomain :execrows
INSERT INTO eregion_platform_domains
    (id,name,environment_id,is_primary,expected_addresses,check_path,created_at,desired_revision,applied_revision,
     route_state,route_error,dns_state,dns_addresses,dns_error,certificate_state,certificate_error,public_state,public_error)
VALUES (sqlc.arg(id)::text,sqlc.arg(name)::text,sqlc.arg(environment_id)::text,
        sqlc.arg(is_primary)::boolean,sqlc.arg(addresses)::text[],sqlc.arg(check_path)::text,
        clock_timestamp(),1,0,'pending','','pending','{}','','pending','','pending','');

-- name: ListPlatformDomains :many
SELECT d.id,d.name,a.name,e.name,d.is_primary,coalesce(d.check_path,''),d.desired_revision,d.applied_revision,
    d.route_state,d.route_error,d.route_applied_at,d.route_observed_at,
    d.expected_addresses,d.dns_state,d.dns_addresses,d.dns_error,d.dns_observed_at,
    d.certificate_state,d.certificate_not_after,d.certificate_error,d.certificate_observed_at,
    d.public_state,coalesce(d.public_status,0),d.public_error,d.public_observed_at,d.observed_at,
    coalesce(dep.instances,'[]'::jsonb),
    coalesce(x.provider,''),coalesce(x.zone_id,''),coalesce(x.zone_name,''),coalesce(x.desired_origins,'{}'),
    coalesce(x.desired_proxied,false),coalesce(x.state,''),coalesce(x.generation,0),coalesce(x.applied_generation,0)
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id
LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=d.id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
ORDER BY a.name,e.name,d.is_primary DESC,d.name
LIMIT sqlc.arg(max_rows)::integer;

-- name: GetPlatformDomainDefinitionSnapshot :many
SELECT d.id,d.name,d.is_primary,coalesce(d.check_path,''),d.desired_revision,
       coalesce(x.zone_id,''),coalesce(x.generation,0),coalesce(x.provider,''),coalesce(x.zone_name,''),
       coalesce(x.ownership_marker,''),coalesce(x.desired_origins,'{}'),coalesce(x.desired_proxied,false),
       coalesce(x.state,''),coalesce(x.applied_generation,0),coalesce(x.last_operation_id,'')
FROM eregion_platform_domains d
LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=d.id
WHERE d.environment_id=sqlc.arg(environment_id)::text
ORDER BY d.name;

-- name: GetPlatformDomainActionFacts :one
SELECT clock_timestamp(),d.id,d.environment_id,d.is_primary,d.desired_revision,d.applied_revision,
       d.route_state,d.route_observed_at,d.route_applied_at,d.dns_state,d.dns_observed_at,
       d.certificate_state,d.certificate_observed_at,d.public_state,d.public_observed_at,
       coalesce(x.provider,''),coalesce(x.zone_name,''),coalesce(x.ownership_marker,''),x.generation,x.applied_generation,
       x.desired_origins,x.desired_proxied,coalesce(x.state,''),coalesce(x.last_operation_id,''),x.updated_at,
       x.cleanup_requested_at,x.cleanup_verified_at
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=d.id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND d.name=sqlc.arg(domain)::text;

-- name: GetPlatformDomainRemovalData :one
SELECT d.id,e.id,d.name,d.is_primary,
       EXISTS(SELECT 1 FROM eregion_platform_domain_dns x WHERE x.domain_id=d.id),
       coalesce((SELECT state FROM eregion_platform_domain_dns x WHERE x.domain_id=d.id),'not_required')
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND d.name=sqlc.arg(domain)::text
FOR UPDATE OF d;

-- name: GetPlatformDomainBindingSummary :one
SELECT a.name,e.name,d.is_primary,d.created_at
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE d.name=sqlc.arg(domain)::text;

-- name: ListPlatformDomainObservationTargets :many
SELECT d.name,d.expected_addresses,
       coalesce(NULLIF(d.check_path,''), NULLIF(r.runtime->>'health_path',''), e.health_path, '/')
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
WHERE d.name > sqlc.arg(after_name)::text
ORDER BY d.name
LIMIT sqlc.arg(max_rows)::integer;

-- name: RecordPlatformDomainObservation :execrows
UPDATE eregion_platform_domains
SET dns_state=sqlc.arg(dns_state)::text,dns_addresses=sqlc.arg(dns_addresses)::text[],
    dns_error=sqlc.arg(dns_error)::text,dns_observed_at=sqlc.narg(dns_observed_at)::timestamptz,
    certificate_state=sqlc.arg(certificate_state)::text,certificate_not_after=sqlc.narg(certificate_not_after)::timestamptz,
    certificate_error=sqlc.arg(certificate_error)::text,certificate_observed_at=sqlc.narg(certificate_observed_at)::timestamptz,
    public_state=sqlc.arg(public_state)::text,public_status=sqlc.narg(public_status)::integer,
    public_error=sqlc.arg(public_error)::text,public_observed_at=sqlc.narg(public_observed_at)::timestamptz,
    observed_at=sqlc.narg(observed_at)::timestamptz
WHERE name=sqlc.arg(domain)::text;

-- name: BumpPlatformDomainDesiredRevisionForEnvironment :execrows
UPDATE eregion_platform_domains
SET desired_revision=desired_revision+1
WHERE environment_id=sqlc.arg(environment_id)::text;

-- name: ListPlatformRouteTargets :many
SELECT d.name,e.id,d.is_primary,
       coalesce(target.instances,'[]'::jsonb)
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
LEFT JOIN eregion_platform_deployment_owners o ON o.environment_id=e.id
LEFT JOIN eregion_platform_deployments target
    ON target.id=CASE
        WHEN o.environment_id IS NOT NULL THEN o.route_target_deployment_id
        ELSE e.serving_deployment_id
    END
ORDER BY d.name;

-- name: MarkPlatformRoutesApplied :execrows
UPDATE eregion_platform_domains
SET route_state='applied',route_error='',applied_revision=desired_revision,
    route_applied_at=coalesce(route_applied_at,sqlc.arg(at)::timestamptz),
    route_observed_at=sqlc.arg(at)::timestamptz,observed_at=sqlc.arg(at)::timestamptz
WHERE name=ANY(sqlc.arg(names)::text[]);

-- name: MarkPlatformRoutesFailed :execrows
UPDATE eregion_platform_domains
SET route_state='failed',route_error=sqlc.arg(message)::text,
    route_observed_at=sqlc.arg(at)::timestamptz,observed_at=sqlc.arg(at)::timestamptz
WHERE name=ANY(sqlc.arg(names)::text[]);

-- name: MarkPlatformRoutesPending :execrows
UPDATE eregion_platform_domains
SET route_state='pending',route_error=sqlc.arg(message)::text,
    route_observed_at=sqlc.arg(at)::timestamptz,observed_at=sqlc.arg(at)::timestamptz
WHERE name=ANY(sqlc.arg(names)::text[]);

-- name: LockPlatformRouteWrite :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: DeletePlatformDomainsForEnvironment :execrows
DELETE FROM eregion_platform_domains WHERE environment_id=sqlc.arg(environment_id)::text;

-- name: DeletePlatformDomainsForEnvironments :execrows
DELETE FROM eregion_platform_domains WHERE environment_id = ANY(sqlc.arg(environment_ids)::text[]);

-- name: DeletePlatformDomainAndDNSCleanup :execrows
DELETE FROM eregion_platform_domain_dns x
USING eregion_platform_domains d
WHERE x.domain_id=d.id AND d.environment_id = ANY(sqlc.arg(environment_ids)::text[])
  AND x.state IN ('cleanup_pending','cleanup_verified','cleanup_failed');

-- ---------------------------------------------------------------- domain dns

-- name: GetPlatformDomainDNSByAppEnvironmentDomain :one
SELECT x.domain_id,d.name,a.name,e.name,x.provider,x.zone_id,x.zone_name,
    x.ownership_marker,x.generation,x.desired_origins,x.desired_proxied,x.state,x.last_operation_id,
    x.last_error,x.applied_generation,x.cleanup_error
FROM eregion_platform_domain_dns x
JOIN eregion_platform_domains d ON d.id=x.domain_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND d.name=sqlc.arg(domain)::text;

-- name: GetPlatformDomainDNSByDomainID :one
SELECT x.domain_id,d.name,a.name,e.name,x.provider,x.zone_id,x.zone_name,
    x.ownership_marker,x.generation,x.desired_origins,x.desired_proxied,x.state,x.last_operation_id,
    x.last_error,x.applied_generation,x.cleanup_error
FROM eregion_platform_domain_dns x
JOIN eregion_platform_domains d ON d.id=x.domain_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE x.domain_id=sqlc.arg(domain_id)::text;

-- name: ListPlatformDomainDNSForEnvironments :many
SELECT x.domain_id,d.name,a.name,e.name,x.provider,x.zone_id,x.zone_name,
    x.ownership_marker,x.generation,x.desired_origins,x.desired_proxied,x.state,x.last_operation_id,
    x.last_error,x.applied_generation,x.cleanup_error
FROM eregion_platform_domain_dns x
JOIN eregion_platform_domains d ON d.id=x.domain_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE e.id = ANY(sqlc.arg(environment_ids)::text[])
ORDER BY a.name,e.name,d.name;

-- name: ListPlatformDomainDNSCleanupViolations :many
SELECT d.name,x.zone_id,x.zone_name,x.state,x.applied_generation
FROM eregion_platform_domain_dns x
JOIN eregion_platform_domains d ON d.id=x.domain_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text);

-- name: LockPlatformDomainDNSLedger :one
SELECT zone_id FROM eregion_platform_domain_dns WHERE domain_id=sqlc.arg(domain_id)::text FOR UPDATE;

-- name: UpdatePlatformDomainDNSIntent :execrows
UPDATE eregion_platform_domain_dns
SET generation=generation+1,desired_origins=sqlc.arg(origins)::text[],
    desired_proxied=sqlc.arg(proxied)::boolean,state='pending',
    last_operation_id=sqlc.arg(operation_id)::text,last_error='',
    cleanup_requested_at=NULL,cleanup_verified_at=NULL,cleanup_error='',updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text;

-- name: InsertPlatformDomainDNSIntent :execrows
INSERT INTO eregion_platform_domain_dns
    (domain_id,provider,zone_id,zone_name,ownership_marker,generation,desired_origins,desired_proxied,state,last_operation_id,last_error,applied_generation,cleanup_error,created_at,updated_at)
VALUES (sqlc.arg(domain_id)::text,'cloudflare',sqlc.arg(zone_id)::text,sqlc.arg(zone_name)::text,
        sqlc.arg(ownership_marker)::text,1,sqlc.arg(origins)::text[],sqlc.arg(proxied)::boolean,
        'pending',sqlc.arg(operation_id)::text,'',0,'',clock_timestamp(),clock_timestamp());

-- name: FencePlatformDomainDNSOperation :execrows
UPDATE eregion_platform_domain_dns
SET last_operation_id=sqlc.arg(operation_id)::text,updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text;

-- name: InsertPlatformDomainDNSCleanupIntent :execrows
INSERT INTO eregion_platform_domain_dns
    (domain_id,provider,zone_id,zone_name,ownership_marker,generation,desired_origins,desired_proxied,
     state,last_operation_id,cleanup_requested_at,last_error,applied_generation,cleanup_error,created_at,updated_at)
VALUES (sqlc.arg(domain_id)::text,'cloudflare',sqlc.arg(zone_id)::text,sqlc.arg(zone_name)::text,
        sqlc.arg(ownership_marker)::text,1,'{}',false,'cleanup_pending',sqlc.arg(operation_id)::text,
        clock_timestamp(),'',0,'',clock_timestamp(),clock_timestamp());

-- name: MarkPlatformDomainDNSCleanupPending :execrows
UPDATE eregion_platform_domain_dns
SET state='cleanup_pending',last_operation_id=sqlc.arg(operation_id)::text,
    cleanup_requested_at=clock_timestamp(),cleanup_verified_at=NULL,
    last_error='',cleanup_error='',updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text;

-- name: MarkPlatformDomainDNSCleanupPendingForEnvironments :execrows
UPDATE eregion_platform_domain_dns x
SET state='cleanup_pending',last_operation_id=sqlc.arg(operation_id)::text,
    cleanup_requested_at=clock_timestamp(),cleanup_verified_at=NULL,
    last_error='',cleanup_error='',updated_at=clock_timestamp()
FROM eregion_platform_domains d
JOIN eregion_platform_environments e ON e.id=d.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE d.id=x.domain_id AND a.name=sqlc.arg(app)::text
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text);

-- name: MarkPlatformDomainDNSCleanupVerified :execrows
UPDATE eregion_platform_domain_dns
SET state='cleanup_verified',last_operation_id=sqlc.arg(operation_id)::text,
    cleanup_verified_at=clock_timestamp(),cleanup_error='',updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text AND last_operation_id=sqlc.arg(operation_id)::text;

-- name: MarkPlatformDomainDNSCleanupVerifiedForGeneration :execrows
UPDATE eregion_platform_domain_dns
SET state='cleanup_verified',cleanup_verified_at=clock_timestamp(),cleanup_error='',updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text AND last_operation_id=sqlc.arg(operation_id)::text
  AND generation=sqlc.arg(generation)::bigint
  AND state IN ('cleanup_pending','cleanup_failed','cleanup_verified');

-- name: MarkPlatformDomainDNSCleanupFailed :execrows
UPDATE eregion_platform_domain_dns
SET state='cleanup_failed',last_operation_id=sqlc.arg(operation_id)::text,
    cleanup_error=sqlc.arg(message)::text,updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text AND last_operation_id=sqlc.arg(operation_id)::text;

-- name: MarkPlatformDomainDNSApplied :execrows
UPDATE eregion_platform_domain_dns
SET state='applied',applied_generation=generation,applied_at=clock_timestamp(),
    last_error='',updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text AND last_operation_id=sqlc.arg(operation_id)::text;

-- name: MarkPlatformDomainDNSFailed :execrows
UPDATE eregion_platform_domain_dns
SET state='failed',last_error=sqlc.arg(message)::text,updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text AND last_operation_id=sqlc.arg(operation_id)::text
  AND generation=sqlc.arg(generation)::bigint;

-- name: SetPlatformDomainDNSCleanupError :execrows
UPDATE eregion_platform_domain_dns
SET cleanup_error=sqlc.arg(message)::text,updated_at=clock_timestamp()
WHERE domain_id=sqlc.arg(domain_id)::text AND last_operation_id=sqlc.arg(operation_id)::text
  AND generation=sqlc.arg(generation)::bigint AND state='cleanup_pending' AND cleanup_error='';

-- name: ListPlatformDomainDNSCredentialZoneIDs :many
SELECT zone_id
FROM eregion_provider_credentials
WHERE provider='cloudflare'
  AND (zone_id=(SELECT zone_id FROM eregion_definition_dns_policy WHERE singleton=true)
       OR zone_id IN (SELECT x.zone_id FROM eregion_platform_domain_dns x
                      JOIN eregion_platform_domains d ON d.id=x.domain_id
                      WHERE d.environment_id=sqlc.arg(environment_id)::text))
ORDER BY zone_id
FOR SHARE;

-- name: ListPlatformDomainDNSZoneIDsForEnvironment :many
SELECT DISTINCT x.zone_id
FROM eregion_platform_domain_dns x
JOIN eregion_platform_domains d ON d.id=x.domain_id
WHERE d.environment_id=sqlc.arg(environment_id)::text
ORDER BY x.zone_id;

-- name: GetPlatformDomainDNSStateForDefinitionStep :one
SELECT coalesce(x.state,''),coalesce(x.last_error,''),coalesce(x.cleanup_error,''),coalesce(x.last_operation_id,'')
FROM eregion_platform_domain_dns x
WHERE x.domain_id=sqlc.arg(domain_id)::text;

-- name: PlatformDomainDNSExistsForOperation :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_domain_dns WHERE last_operation_id=sqlc.arg(operation_id)::text);

-- name: PlatformDomainDNSExistsForDomain :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_domain_dns WHERE domain_id=sqlc.arg(domain_id)::text);

-- name: DeletePlatformDomainDNSForBinding :execrows
DELETE FROM eregion_platform_domain_dns x
USING eregion_platform_domains d
WHERE x.domain_id=d.id AND d.name=sqlc.arg(domain)::text
  AND d.environment_id=sqlc.arg(environment_id)::text;

-- name: DeletePlatformDomainDNSCleanupForEnvironments :execrows
DELETE FROM eregion_platform_domain_dns x
USING eregion_platform_domains d
WHERE x.domain_id=d.id AND d.environment_id = ANY(sqlc.arg(environment_ids)::text[])
  AND x.state IN ('cleanup_pending','cleanup_verified','cleanup_failed');

-- ---------------------------------------------------------------- deployment owners

-- name: GetPlatformDeploymentOwner :one
SELECT coalesce(o.operation_id,''),coalesce(o.deployment_id,''),
       coalesce(o.owner_attempt,0),o.phase,coalesce(o.route_target_deployment_id,''),
       coalesce(o.restore_release_id,''),coalesce(o.restore_deployment_id,'')
FROM eregion_platform_deployment_owners o
WHERE o.environment_id=sqlc.arg(environment_id)::text;

-- name: PlatformDeploymentOwnerExists :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_deployment_owners WHERE environment_id=sqlc.arg(environment_id)::text);

-- name: CountPlatformDeploymentOwners :one
SELECT count(*)
FROM eregion_platform_deployment_owners o
JOIN eregion_platform_environments e ON e.id=o.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text);

-- name: InsertPlatformDeploymentOwner :execrows
INSERT INTO eregion_platform_deployment_owners
    (environment_id,operation_id,deployment_id,owner_attempt,phase,route_target_deployment_id,restore_release_id,restore_deployment_id,created_at,updated_at)
VALUES (sqlc.arg(environment_id)::text,sqlc.arg(operation_id)::text,sqlc.arg(deployment_id)::text,
        sqlc.arg(owner_attempt)::integer,sqlc.arg(phase)::text,sqlc.narg(route_target)::text,
        sqlc.narg(restore_release)::text,sqlc.narg(restore_deployment)::text,
        clock_timestamp(),clock_timestamp())
ON CONFLICT (environment_id) DO NOTHING;

-- name: RenewPlatformDeploymentOwner :execrows
UPDATE eregion_platform_deployment_owners
SET owner_attempt=sqlc.arg(owner_attempt)::integer,updated_at=clock_timestamp()
WHERE environment_id=sqlc.arg(environment_id)::text
  AND operation_id=sqlc.arg(operation_id)::text
  AND owner_attempt <= sqlc.arg(owner_attempt)::integer;

-- name: SetPlatformDeploymentOwnerRoute :one
UPDATE eregion_platform_deployment_owners o
SET route_target_deployment_id=sqlc.narg(route_target)::text,phase=sqlc.arg(phase)::text,
    owner_attempt=sqlc.arg(owner_attempt)::integer,updated_at=clock_timestamp()
WHERE o.environment_id=sqlc.arg(environment_id)::text
  AND o.operation_id=sqlc.arg(operation_id)::text
  AND o.deployment_id=sqlc.arg(deployment_id)::text
  AND o.owner_attempt <= sqlc.arg(owner_attempt)::integer
RETURNING coalesce(o.operation_id,''),coalesce(o.deployment_id,''),
          coalesce(o.owner_attempt,0),o.phase,coalesce(o.route_target_deployment_id,''),
          coalesce(o.restore_release_id,''),coalesce(o.restore_deployment_id,'');

-- name: ListPlatformDeploymentOwnersToSettle :many
SELECT o.environment_id,coalesce(o.operation_id,''),coalesce(o.deployment_id,''),
       coalesce(o.owner_attempt,0),o.phase,coalesce(o.route_target_deployment_id,''),
       coalesce(o.restore_release_id,''),coalesce(o.restore_deployment_id,''),a.name,e.name
FROM eregion_platform_deployment_owners o
JOIN eregion_platform_environments e ON e.id=o.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_operations op ON op.id=o.operation_id
WHERE o.phase='ambiguous' OR op.id IS NULL OR op.state NOT IN ('queued','running','cancelling')
ORDER BY o.environment_id
LIMIT 100;

-- name: GetPlatformDeploymentCleanupProtected :one
SELECT coalesce(e.serving_deployment_id,''),coalesce(o.phase,''),coalesce(o.deployment_id,''),
       coalesce(o.route_target_deployment_id,''),coalesce(o.restore_deployment_id,''),o.environment_id IS NOT NULL
FROM eregion_platform_environments e
LEFT JOIN eregion_platform_deployment_owners o ON o.environment_id=e.id
WHERE e.id=sqlc.arg(environment_id)::text;

-- name: ResolvePlatformAmbiguousDeploymentOwner :execrows
UPDATE eregion_platform_deployment_owners
SET route_target_deployment_id=sqlc.narg(route_target)::text,updated_at=clock_timestamp()
WHERE environment_id=sqlc.arg(environment_id)::text AND operation_id IS NULL;

-- name: ListPlatformActiveDeploymentOwnersForEnvironment :many
SELECT o.id,o.attempt,o.state,d.id
FROM eregion_operations o
JOIN eregion_platform_deployments d ON d.operation_id=o.id
WHERE d.environment_id=sqlc.arg(environment_id)::text
  AND o.state IN ('queued','running','cancelling')
  AND o.kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote')
ORDER BY o.id;

-- name: CountPlatformActiveDeploymentOwnersForEnvironment :one
SELECT count(*)
FROM eregion_operations o
JOIN eregion_platform_deployments d ON d.operation_id=o.id
WHERE d.environment_id=sqlc.arg(environment_id)::text
  AND o.state IN ('queued','running','cancelling')
  AND o.kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote');

-- name: DeletePlatformAmbiguousDeploymentOwner :execrows
DELETE FROM eregion_platform_deployment_owners
WHERE environment_id=sqlc.arg(environment_id)::text AND operation_id IS NULL;

-- name: DeletePlatformRestoredDeploymentOwnerByOperation :execrows
DELETE FROM eregion_platform_deployment_owners o
USING eregion_platform_environments e
WHERE o.operation_id=sqlc.arg(operation_id)::text AND o.environment_id=e.id
  AND o.phase IN ('prepared','restored')
  AND o.route_target_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id
  AND e.serving_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id;

-- name: DeletePlatformServedDeploymentOwnerByOperation :execrows
DELETE FROM eregion_platform_deployment_owners o
USING eregion_platform_environments e
WHERE o.operation_id=sqlc.arg(operation_id)::text AND o.environment_id=e.id
  AND o.phase='serving'
  AND o.route_target_deployment_id=o.deployment_id
  AND e.serving_deployment_id=o.deployment_id;

-- name: DeletePlatformRestoredDeploymentOwner :execrows
DELETE FROM eregion_platform_deployment_owners o
USING eregion_platform_environments e
WHERE o.environment_id=sqlc.arg(environment_id)::text AND o.operation_id=sqlc.arg(operation_id)::text
  AND o.owner_attempt=sqlc.arg(owner_attempt)::integer
  AND o.environment_id=e.id
  AND o.phase IN ('prepared','restored')
  AND o.route_target_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id
  AND e.serving_deployment_id IS NOT DISTINCT FROM o.restore_deployment_id;

-- name: DeletePlatformServedDeploymentOwner :execrows
DELETE FROM eregion_platform_deployment_owners o
USING eregion_platform_environments e
WHERE o.environment_id=sqlc.arg(environment_id)::text AND o.operation_id=sqlc.arg(operation_id)::text
  AND o.owner_attempt=sqlc.arg(owner_attempt)::integer
  AND o.environment_id=e.id
  AND o.phase='serving'
  AND o.route_target_deployment_id=o.deployment_id
  AND e.serving_deployment_id=o.deployment_id;

-- ---------------------------------------------------------------- container cleanup

-- name: RecordPlatformContainerCleanup :one
INSERT INTO eregion_platform_container_cleanup
    (container_name,environment_id,node,deployment_id,reason,attempts,last_error,next_attempt_at,created_at)
VALUES (sqlc.arg(container_name)::text,sqlc.arg(environment_id)::text,sqlc.arg(node)::text,
        sqlc.narg(deployment_id)::text,sqlc.arg(reason)::text,1,sqlc.arg(last_error)::text,
        clock_timestamp(),clock_timestamp())
ON CONFLICT (container_name) DO UPDATE
SET environment_id=EXCLUDED.environment_id,node=EXCLUDED.node,
    deployment_id=EXCLUDED.deployment_id,reason=EXCLUDED.reason,
    attempts=eregion_platform_container_cleanup.attempts+1,
    last_error=EXCLUDED.last_error,next_attempt_at=clock_timestamp()
RETURNING attempts;

-- name: EnsurePlatformContainerCleanup :execrows
INSERT INTO eregion_platform_container_cleanup
    (container_name,environment_id,node,deployment_id,reason,attempts,last_error,next_attempt_at,created_at)
VALUES (sqlc.arg(container_name)::text,sqlc.arg(environment_id)::text,sqlc.arg(node)::text,
        sqlc.narg(deployment_id)::text,sqlc.arg(reason)::text,1,sqlc.arg(last_error)::text,
        clock_timestamp(),clock_timestamp())
ON CONFLICT (container_name) DO UPDATE
SET environment_id=EXCLUDED.environment_id,node=EXCLUDED.node,
    deployment_id=EXCLUDED.deployment_id,reason=EXCLUDED.reason,
    last_error=EXCLUDED.last_error;

-- name: ReschedulePlatformContainerCleanup :execrows
UPDATE eregion_platform_container_cleanup
SET next_attempt_at=clock_timestamp()+(sqlc.arg(delay_seconds)::integer * interval '1 second')
WHERE container_name=sqlc.arg(container_name)::text;

-- name: ListDuePlatformContainerCleanup :many
SELECT c.container_name,c.environment_id,a.name,e.name,c.node,
       coalesce(c.deployment_id,''),c.reason,c.attempts,c.created_at
FROM eregion_platform_container_cleanup c
JOIN eregion_platform_environments e ON e.id=c.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE c.next_attempt_at <= clock_timestamp()
ORDER BY c.next_attempt_at,c.container_name
LIMIT sqlc.arg(max_rows)::integer;

-- name: ListPlatformContainerCleanupForDeployment :many
SELECT c.container_name,c.environment_id,a.name,e.name,c.node,
       coalesce(c.deployment_id,''),c.reason,c.attempts,c.created_at
FROM eregion_platform_container_cleanup c
JOIN eregion_platform_environments e ON e.id=c.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE c.environment_id=sqlc.arg(environment_id)::text AND c.deployment_id=sqlc.arg(deployment_id)::text
  AND (sqlc.narg(reason)::text IS NULL OR c.reason=sqlc.narg(reason)::text)
ORDER BY c.container_name;

-- name: DeletePlatformContainerCleanup :execrows
DELETE FROM eregion_platform_container_cleanup WHERE container_name=sqlc.arg(container_name)::text;

-- ---------------------------------------------------------------- release commands

-- name: GetPlatformReleaseCommandRun :one
SELECT r.state,coalesce(r.operation_id,''),
       EXISTS(SELECT 1 FROM eregion_operations o
              WHERE o.id=r.operation_id AND o.state IN ('queued','running','cancelling'))
FROM eregion_platform_release_command_runs r
WHERE r.release_id=sqlc.arg(release_id)::text AND r.command_digest=sqlc.arg(command_digest)::text;

-- name: StartPlatformReleaseCommandRun :execrows
INSERT INTO eregion_platform_release_command_runs
    (release_id,command_digest,operation_id,state,started_at,error_code,error)
VALUES (sqlc.arg(release_id)::text,sqlc.arg(command_digest)::text,sqlc.arg(operation_id)::text,
        'running',clock_timestamp(),'','')
ON CONFLICT (release_id,command_digest) DO UPDATE
SET operation_id=EXCLUDED.operation_id,state='running',error_code='',error='',
    started_at=clock_timestamp(),completed_at=NULL
WHERE eregion_platform_release_command_runs.operation_id=EXCLUDED.operation_id
   OR eregion_platform_release_command_runs.state='failed';

-- name: FinishPlatformReleaseCommandRun :execrows
UPDATE eregion_platform_release_command_runs
SET state=sqlc.arg(state)::text,error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE release_id=sqlc.arg(release_id)::text AND command_digest=sqlc.arg(command_digest)::text
  AND operation_id=sqlc.arg(operation_id)::text AND state='running';

-- name: FinishRecoveredPlatformReleaseCommandRun :execrows
UPDATE eregion_platform_release_command_runs
SET state=sqlc.arg(state)::text,error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE release_id=sqlc.arg(release_id)::text AND command_digest=sqlc.arg(command_digest)::text
  AND state='running' AND operation_id=sqlc.arg(operation_id)::text;

-- name: GetPlatformReleaseCommandForDeployment :one
SELECT state,coalesce(operation_id,'')
FROM eregion_platform_release_command_runs
WHERE release_id=sqlc.arg(release_id)::text AND command_digest=sqlc.arg(command_digest)::text;

-- name: DeletePlatformReleaseCommandRuns :execrows
DELETE FROM eregion_platform_release_command_runs WHERE release_id=sqlc.arg(release_id)::text;

-- name: GetPlatformDefinitionCleanupCounts :one
SELECT
    count(*) FILTER (WHERE s.kind=sqlc.arg(success_kind)::text AND c.state='succeeded'),
    count(*) FILTER (WHERE s.kind=sqlc.arg(authz_kind)::text AND c.state='succeeded'),
    coalesce((SELECT CASE WHEN failed.kind=sqlc.arg(failed_kind)::text THEN x.cleanup_error ELSE x.last_error END
              FROM eregion_definition_steps failed
              JOIN eregion_platform_domain_dns x ON x.domain_id=failed.domain_id
              WHERE failed.parent_operation_id=sqlc.arg(parent_operation_id)::text
                AND failed.ordinal=sqlc.arg(ordinal)::integer
                AND x.last_operation_id=sqlc.arg(operation_id)::text),'')
FROM eregion_definition_steps s
JOIN eregion_operations c ON c.id=s.child_operation_id
WHERE s.parent_operation_id=sqlc.arg(parent_operation_id)::text
  AND s.ordinal<sqlc.arg(ordinal)::integer;

-- name: DeletePlatformDomainBinding :execrows
DELETE FROM eregion_platform_domains d
USING eregion_platform_environments e, eregion_platform_apps a
WHERE d.environment_id=e.id AND e.app_id=a.id
  AND d.name=sqlc.arg(domain)::text AND a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text;
