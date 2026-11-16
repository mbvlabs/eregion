-- Platform pointer, resource and node persistence for the entities foundation.

-- ---------------------------------------------------------------- pointers

-- name: ListPlatformOperationChildPlacements :many
SELECT kind,id,app_id,environment_id FROM (
    SELECT 'build' AS kind,id,app_id,environment_id FROM eregion_platform_builds WHERE operation_id=sqlc.arg(operation_id)::text
    UNION ALL SELECT 'release',id,app_id,environment_id FROM eregion_platform_releases WHERE operation_id=sqlc.arg(operation_id)::text
    UNION ALL SELECT 'deployment',id,app_id,environment_id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text
) x ORDER BY kind,id;

-- name: PlatformDeploymentMatchesOperation :one
SELECT EXISTS (
  SELECT 1 FROM eregion_platform_deployments
  WHERE operation_id=sqlc.arg(operation_id)::text AND id=sqlc.arg(deployment_id)::text
    AND environment_id=sqlc.arg(environment_id)::text AND release_id=sqlc.arg(release_id)::text);

-- name: GetPlatformBuildOwnership :one
SELECT app_id,environment_id,COALESCE(release_id,'')
FROM eregion_platform_builds
WHERE id=sqlc.arg(id)::text;

-- name: GetPlatformReleaseBuildID :one
SELECT build_id FROM eregion_platform_releases WHERE id=sqlc.arg(id)::text;

-- name: LockPlatformEnvironmentPointers :one
SELECT app_id,current_revision_id,desired_release_id,applied_release_id,serving_release_id,serving_deployment_id
FROM eregion_platform_environments
WHERE id=sqlc.arg(id)::text
FOR UPDATE;

-- name: LockPlatformAppKeyShare :one
SELECT id FROM eregion_platform_apps WHERE id=sqlc.arg(id)::text FOR KEY SHARE;

-- name: LockPlatformRevisionPointer :one
SELECT environment_id
FROM eregion_platform_environment_revisions
WHERE id=sqlc.arg(id)::text AND environment_id=sqlc.arg(environment_id)::text
FOR KEY SHARE;

-- name: LockPlatformOwnedRelease :one
SELECT app_id,environment_id,pruned_at IS NOT NULL,build_id
FROM eregion_platform_releases
WHERE id=sqlc.arg(id)::text AND app_id=sqlc.arg(app_id)::text AND environment_id=sqlc.arg(environment_id)::text
FOR KEY SHARE;

-- name: LockPlatformOwnedBuildForRelease :one
SELECT app_id,environment_id,COALESCE(release_id,'')
FROM eregion_platform_builds
WHERE id=sqlc.arg(id)::text AND app_id=sqlc.arg(app_id)::text AND environment_id=sqlc.arg(environment_id)::text
FOR KEY SHARE;

-- name: LockPlatformOwnedDeployment :one
SELECT app_id,environment_id,release_id
FROM eregion_platform_deployments
WHERE id=sqlc.arg(id)::text AND app_id=sqlc.arg(app_id)::text AND environment_id=sqlc.arg(environment_id)::text
FOR KEY SHARE;

-- name: SetPlatformReleaseBuildPointer :execrows
UPDATE eregion_platform_releases SET build_id=sqlc.arg(build_id)::text WHERE id=sqlc.arg(id)::text;

-- name: TryPlatformTargetXactLock :one
SELECT pg_try_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: TryPlatformSessionLock :one
SELECT pg_try_advisory_lock(sqlc.arg(key)::bigint);

-- name: AcquirePlatformSessionLock :exec
SELECT pg_advisory_lock(sqlc.arg(key)::bigint);

-- name: ReleasePlatformSessionLock :one
SELECT pg_advisory_unlock(sqlc.arg(key)::bigint);

-- name: LockPlatformWireGuardAllocation :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- ---------------------------------------------------------------- resources

-- name: GetPlatformResourceByName :one
SELECT r.id,a.name,e.name,r.name,r.node_name,r.spec,r.state,r.image,r.image_id,r.image_digest,
    r.container_name,r.network_name,r.volumes,r.health_state,r.health_detail,r.health_checked_at,
    coalesce(r.operation_id,''),r.error_code,r.error,r.created_at,r.updated_at,r.destroyed_at
FROM eregion_platform_resources r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND r.name=sqlc.arg(name)::text;

-- name: GetPlatformResourceByNameForUpdate :one
SELECT r.id,a.name,e.name,r.name,r.node_name,r.spec,r.state,r.image,r.image_id,r.image_digest,
    r.container_name,r.network_name,r.volumes,r.health_state,r.health_detail,r.health_checked_at,
    coalesce(r.operation_id,''),r.error_code,r.error,r.created_at,r.updated_at,r.destroyed_at
FROM eregion_platform_resources r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND r.name=sqlc.arg(name)::text
FOR UPDATE OF r;

-- name: LockPlatformResourceState :one
SELECT state FROM eregion_platform_resources
WHERE environment_id=sqlc.arg(environment_id)::text AND name=sqlc.arg(name)::text
FOR UPDATE;

-- name: ListPlatformResources :many
SELECT r.id,a.name,e.name,r.name,r.node_name,r.spec,r.state,r.image,r.image_id,r.image_digest,
    r.container_name,r.network_name,r.volumes,r.health_state,r.health_detail,r.health_checked_at,
    coalesce(r.operation_id,''),r.error_code,r.error,r.created_at,r.updated_at,r.destroyed_at
FROM eregion_platform_resources r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(state)::text IS NULL OR r.state=sqlc.narg(state)::text)
  AND (sqlc.narg(node)::text IS NULL OR r.node_name=sqlc.narg(node)::text)
ORDER BY a.name,e.name,r.name
LIMIT sqlc.arg(max_rows)::integer;

-- name: InsertPlatformResource :execrows
INSERT INTO eregion_platform_resources
    (id,app_id,environment_id,name,node_name,spec,state,image,operation_id,image_id,image_digest,container_name,network_name,volumes,health_state,health_detail,error_code,error,created_at,updated_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(app_id)::text,sqlc.arg(environment_id)::text,sqlc.arg(name)::text,
        sqlc.arg(node_name)::text,sqlc.arg(spec)::jsonb,'pending',sqlc.arg(image)::text,
        sqlc.arg(operation_id)::text,'','','','','[]'::jsonb,'unknown','','','',
        clock_timestamp(),clock_timestamp());

-- name: ResetPlatformResourceForApply :execrows
UPDATE eregion_platform_resources
SET node_name=sqlc.arg(node_name)::text,spec=sqlc.arg(spec)::jsonb,image=sqlc.arg(image)::text,
    state='pending',operation_id=sqlc.arg(operation_id)::text,
    health_state='unknown',health_detail='',health_checked_at=NULL,
    error_code='',error='',destroyed_at=NULL,updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: MarkPlatformResourceDestroying :execrows
UPDATE eregion_platform_resources
SET state='destroying',operation_id=sqlc.arg(operation_id)::text,error_code='',error='',updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: MarkPlatformResourceReady :execrows
UPDATE eregion_platform_resources
SET state='ready',image=sqlc.arg(image)::text,image_id=sqlc.arg(image_id)::text,
    image_digest=sqlc.arg(image_digest)::text,container_name=sqlc.arg(container_name)::text,
    network_name=sqlc.arg(network_name)::text,volumes=sqlc.arg(volumes)::jsonb,
    health_state='healthy',health_detail='',health_checked_at=clock_timestamp(),
    operation_id=sqlc.arg(operation_id)::text,error_code='',error='',destroyed_at=NULL,
    updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text;

-- name: MarkPlatformResourceDestroyed :execrows
UPDATE eregion_platform_resources
SET state='destroyed',container_name='',volumes=sqlc.arg(volumes)::jsonb,health_state='unknown',
    health_detail=sqlc.arg(detail)::text,health_checked_at=clock_timestamp(),
    operation_id=sqlc.arg(operation_id)::text,error_code='',error='',
    destroyed_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text;

-- name: RecordPlatformResourceContainer :execrows
UPDATE eregion_platform_resources
SET image=sqlc.arg(image)::text,image_id=sqlc.arg(image_id)::text,image_digest=sqlc.arg(image_digest)::text,
    container_name=sqlc.arg(container_name)::text,network_name=sqlc.arg(network_name)::text,
    volumes=sqlc.arg(volumes)::jsonb,updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text;

-- name: LockPlatformResourceVolumes :one
SELECT volumes FROM eregion_platform_resources
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text
FOR UPDATE;

-- name: SetPlatformResourceVolumes :execrows
UPDATE eregion_platform_resources
SET volumes=sqlc.arg(volumes)::jsonb,updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text;

-- name: SettlePlatformResourceForTerminalOperation :execrows
UPDATE eregion_platform_resources
SET state='degraded',health_state='unknown',health_detail=sqlc.arg(detail)::text,
    health_checked_at=clock_timestamp(),error_code=sqlc.arg(error_code)::text,error=sqlc.arg(detail)::text,
    updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('provisioning','destroying');

-- name: ReconcileTerminalPlatformResources :execrows
UPDATE eregion_platform_resources r
SET state='degraded', health_state='unknown',
    health_detail=CASE r.state
        WHEN 'destroying' THEN 'Resource destroy was interrupted before a durable destroyed state; re-apply or re-destroy the resource explicitly.'
        ELSE 'Resource apply was interrupted before a durable ready state; re-apply or destroy the resource explicitly.'
    END,
    health_checked_at=clock_timestamp(),
    error_code=CASE WHEN o.error_code<>'' THEN o.error_code ELSE 'cancelled' END,
    error=CASE r.state
        WHEN 'destroying' THEN 'Resource destroy was interrupted before a durable destroyed state; re-apply or re-destroy the resource explicitly.'
        ELSE 'Resource apply was interrupted before a durable ready state; re-apply or destroy the resource explicitly.'
    END,
    updated_at=clock_timestamp()
FROM eregion_operations o
WHERE r.operation_id=o.id AND o.state IN ('cancelled','failed')
  AND r.state IN ('provisioning','destroying');

-- name: RecordPlatformResourceFailure :execrows
UPDATE eregion_platform_resources r
SET state='degraded', health_state='unhealthy', health_detail=sqlc.arg(message)::text,
    health_checked_at=clock_timestamp(), error_code=sqlc.arg(error_code)::text, error=sqlc.arg(message)::text,
    updated_at=clock_timestamp()
FROM eregion_platform_apps a, eregion_platform_environments e
WHERE r.app_id=a.id AND r.environment_id=e.id
  AND a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND r.name=sqlc.arg(name)::text
  AND r.operation_id=sqlc.arg(operation_id)::text;

-- name: SetPlatformResourceStateForTarget :execrows
UPDATE eregion_platform_resources r
SET state=sqlc.arg(state)::text, updated_at=clock_timestamp()
FROM eregion_platform_apps a, eregion_platform_environments e
WHERE r.app_id=a.id AND r.environment_id=e.id
  AND 'resource/'||a.name||'/'||e.name||'/'||r.name=sqlc.arg(target)::text
  AND r.operation_id=sqlc.arg(operation_id)::text;

-- name: GetPlatformResourceDeployability :one
SELECT state, health_state
FROM eregion_platform_resources
WHERE environment_id=sqlc.arg(environment_id)::text AND name=sqlc.arg(name)::text;

-- name: ListPlatformResourceTargets :many
SELECT ('resource/'||a.name||'/'||e.name||'/'||r.name)::text
FROM eregion_platform_resources r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE a.name=sqlc.arg(app)::text
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
ORDER BY r.name;

-- name: ListPlatformLiveResourcesForEnvironment :many
SELECT e.name,r.name,r.state
FROM eregion_platform_resources r
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE r.state<>'destroyed' AND r.environment_id=sqlc.arg(environment_id)::text
ORDER BY e.name,r.name;

-- name: ListPlatformLiveResourcesForApp :many
SELECT e.name,r.name,r.state
FROM eregion_platform_resources r
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE r.state<>'destroyed' AND r.app_id=sqlc.arg(app_id)::text
ORDER BY e.name,r.name;

-- name: GetPlatformResourceDefinitionSnapshot :many
SELECT id,name,node_name,spec,state,coalesce(operation_id,'')
FROM eregion_platform_resources
WHERE environment_id=sqlc.arg(environment_id)::text
ORDER BY name;

-- name: GetPlatformResourceActionFacts :one
SELECT clock_timestamp(),r.updated_at,r.id,r.environment_id,r.node_name,r.state,
       r.image,r.image_id,r.container_name,r.network_name,r.volumes,r.health_state,r.health_checked_at,
       coalesce(r.operation_id,''),r.destroyed_at,r.spec
FROM eregion_platform_resources r
JOIN eregion_platform_environments e ON e.id=r.environment_id
JOIN eregion_platform_apps a ON a.id=r.app_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text AND r.name=sqlc.arg(name)::text;

-- name: DeletePlatformResourcesForEnvironment :execrows
DELETE FROM eregion_platform_resources WHERE environment_id=sqlc.arg(environment_id)::text;

-- ---------------------------------------------------------------- nodes

-- name: GetPlatformNode :one
SELECT name,roles,capabilities,address,status,last_seen_at,created_at,updated_at,
    enrollment_state,enrollment_error,transport,ssh_user,ssh_host_key_fingerprint,identity_fingerprint,
    os_id,os_version,arch,build_concurrency,wg_enabled,wg_address,wg_endpoint,wg_public_key,wg_state,
    wg_reconciled_at,enrolled_at,last_check_at,last_check_state,last_check_detail
FROM eregion_platform_nodes
WHERE name=sqlc.arg(name)::text;

-- name: ListPlatformNodes :many
SELECT name,roles,capabilities,address,status,last_seen_at,created_at,updated_at,
    enrollment_state,enrollment_error,transport,ssh_user,ssh_host_key_fingerprint,identity_fingerprint,
    os_id,os_version,arch,build_concurrency,wg_enabled,wg_address,wg_endpoint,wg_public_key,wg_state,
    wg_reconciled_at,enrolled_at,last_check_at,last_check_state,last_check_detail
FROM eregion_platform_nodes
WHERE (sqlc.narg(name)::text IS NULL OR name=sqlc.narg(name)::text)
  AND (sqlc.narg(state)::text IS NULL OR status=sqlc.narg(state)::text OR enrollment_state=sqlc.narg(state)::text)
ORDER BY name
LIMIT sqlc.arg(max_rows)::integer;

-- name: GetPlatformNodeConnection :one
SELECT name,roles,enrollment_state,transport,ssh_address,ssh_user,ssh_host_key,
    expected_identity_fingerprint,identity_public_key,identity_fingerprint,build_concurrency,build_user,
    wg_enabled,wg_address,wg_endpoint,wg_ssh_port,wg_public_key,control_wg_public_key,wg_state
FROM eregion_platform_nodes WHERE name=sqlc.arg(name)::text;

-- name: PlatformNodeExists :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_nodes WHERE name=sqlc.arg(name)::text);

-- name: InsertPlatformNodeRegistration :execrows
INSERT INTO eregion_platform_nodes(name,roles,capabilities,address,status,created_at,updated_at,
    enrollment_state,enrollment_error,transport,ssh_address,ssh_user,ssh_host_key,ssh_host_key_fingerprint,
    expected_identity_fingerprint,identity_public_key,identity_fingerprint,os_id,os_version,arch,
    build_concurrency,build_user,wg_enabled,wg_address,wg_endpoint,wg_ssh_port,wg_public_key,control_wg_public_key,wg_state,last_check_state,last_check_detail)
VALUES (sqlc.arg(name)::text,sqlc.arg(roles)::text[],sqlc.arg(capabilities)::text[],sqlc.arg(address)::text,
        'registered',clock_timestamp(),clock_timestamp(),'registered','','local','','','','','','','','','','',
        1,'',false,'','',0,'','','none','unknown','')
ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles, capabilities=EXCLUDED.capabilities,
    address=EXCLUDED.address, updated_at=clock_timestamp()
WHERE eregion_platform_nodes.enrollment_state='registered';

-- name: EnsurePlatformLocalNode :execrows
INSERT INTO eregion_platform_nodes(name,roles,capabilities,address,status,last_seen_at,created_at,updated_at,
    enrollment_state,enrollment_error,transport,ssh_address,ssh_user,ssh_host_key,ssh_host_key_fingerprint,
    expected_identity_fingerprint,identity_public_key,identity_fingerprint,os_id,os_version,arch,
    build_concurrency,build_user,wg_enabled,wg_address,wg_endpoint,wg_ssh_port,wg_public_key,control_wg_public_key,wg_state,last_check_state,last_check_detail)
VALUES (sqlc.arg(name)::text,sqlc.arg(roles)::text[],sqlc.arg(capabilities)::text[],'','ready',
        clock_timestamp(),clock_timestamp(),clock_timestamp(),'registered','','local','','','','','','','','','','',
        1,'',false,'','',0,'','','none','unknown','')
ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles,capabilities=EXCLUDED.capabilities,
    status='ready',last_seen_at=clock_timestamp(),updated_at=clock_timestamp();

-- name: ResetPlatformNodeEnrollment :execrows
UPDATE eregion_platform_nodes
SET enrollment_state=CASE WHEN enrollment_state='enrolled' THEN 'enrolled' ELSE 'pending' END,
    enrollment_error='', enroll_operation_id=sqlc.arg(operation_id)::text, updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: UpsertPlatformNodeEnrollment :execrows
INSERT INTO eregion_platform_nodes
    (name,roles,capabilities,address,status,enrollment_state,transport,ssh_address,ssh_user,ssh_host_key,
     ssh_host_key_fingerprint,expected_identity_fingerprint,build_concurrency,build_user,wg_enabled,wg_address,wg_endpoint,wg_ssh_port,enroll_operation_id,
     created_at,updated_at,enrollment_error,identity_public_key,identity_fingerprint,os_id,os_version,arch,wg_public_key,control_wg_public_key,wg_state,last_check_state,last_check_detail)
VALUES (sqlc.arg(name)::text,sqlc.arg(roles)::text[],sqlc.arg(capabilities)::text[],sqlc.arg(address)::text,
        'registered',sqlc.arg(enrollment_state)::text,'ssh',sqlc.arg(address)::text,sqlc.arg(ssh_user)::text,
        sqlc.arg(host_key)::text,sqlc.arg(host_key_fingerprint)::text,sqlc.arg(identity_fingerprint)::text,
        sqlc.arg(build_concurrency)::integer,sqlc.arg(build_user)::text,sqlc.arg(wg_enabled)::boolean,
        sqlc.arg(wg_address)::text,sqlc.arg(wg_endpoint)::text,sqlc.arg(wg_ssh_port)::integer,
        sqlc.arg(operation_id)::text,clock_timestamp(),clock_timestamp(),'','','','','','','','','none','unknown','')
ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles, address=EXCLUDED.address, enrollment_state=EXCLUDED.enrollment_state,
    enrollment_error='', transport='ssh', ssh_address=EXCLUDED.ssh_address, ssh_user=EXCLUDED.ssh_user,
    ssh_host_key=EXCLUDED.ssh_host_key, ssh_host_key_fingerprint=EXCLUDED.ssh_host_key_fingerprint,
    expected_identity_fingerprint=CASE WHEN EXCLUDED.expected_identity_fingerprint<>'' THEN EXCLUDED.expected_identity_fingerprint ELSE eregion_platform_nodes.expected_identity_fingerprint END,
    build_concurrency=EXCLUDED.build_concurrency, build_user=EXCLUDED.build_user, wg_enabled=EXCLUDED.wg_enabled,
    wg_address=EXCLUDED.wg_address, wg_endpoint=EXCLUDED.wg_endpoint, wg_ssh_port=EXCLUDED.wg_ssh_port, enroll_operation_id=EXCLUDED.enroll_operation_id,
    updated_at=clock_timestamp();

-- name: RecordPlatformNodeEnrolled :execrows
UPDATE eregion_platform_nodes SET
    enrollment_state='enrolled', enrollment_error='', enrolled_at=coalesce(enrolled_at,clock_timestamp()),
    identity_public_key=sqlc.arg(identity_public_key)::text, identity_fingerprint=sqlc.arg(identity_fingerprint)::text,
    capabilities=sqlc.arg(capabilities)::text[], os_id=sqlc.arg(os_id)::text, os_version=sqlc.arg(os_version)::text,
    arch=sqlc.arg(arch)::text, probe=sqlc.arg(probe)::jsonb,
    status='ready', last_seen_at=clock_timestamp(), last_check_at=clock_timestamp(),
    last_check_state='ok', last_check_detail=sqlc.arg(detail)::text,
    updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: RecordPlatformNodeEnrollmentFailure :execrows
UPDATE eregion_platform_nodes
SET enrollment_state=CASE WHEN sqlc.arg(terminal)::boolean AND enrollment_state='pending' THEN 'failed' ELSE enrollment_state END,
    enrollment_error=CASE WHEN enrollment_state='enrolled' THEN enrollment_error ELSE sqlc.arg(message)::text END,
    last_check_at=clock_timestamp(), last_check_state=sqlc.arg(check_state)::text,
    last_check_detail=sqlc.arg(message)::text, updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: RecordPlatformNodeCheckSuccess :execrows
UPDATE eregion_platform_nodes SET capabilities=sqlc.arg(capabilities)::text[],
    os_id=sqlc.arg(os_id)::text, os_version=sqlc.arg(os_version)::text, arch=sqlc.arg(arch)::text,
    probe=sqlc.arg(probe)::jsonb, status='ready', last_seen_at=clock_timestamp(),
    last_check_at=clock_timestamp(), last_check_state='ok', last_check_detail=sqlc.arg(detail)::text,
    updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: RecordPlatformNodeCheckStatus :execrows
UPDATE eregion_platform_nodes
SET status=CASE WHEN status='disabled' THEN status ELSE sqlc.arg(status)::text END,
    last_check_at=clock_timestamp(), last_check_state=sqlc.arg(state)::text,
    last_check_detail=sqlc.arg(detail)::text, updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: RecordPlatformNodeCheckReady :execrows
UPDATE eregion_platform_nodes SET status='ready',last_seen_at=clock_timestamp(),last_check_at=clock_timestamp(),
    last_check_state='ok',last_check_detail='docker reachable (declared facts, not enrolled)',updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: GetPlatformNodeEnrollmentForTarget :one
SELECT enrollment_state,enroll_operation_id
FROM eregion_platform_nodes
WHERE name=(SELECT split_part(sqlc.arg(target)::text,'/',2));

-- name: LockPlatformNodeBuildConcurrency :one
SELECT build_concurrency,roles,enrollment_state,transport
FROM eregion_platform_nodes
WHERE name=sqlc.arg(name)::text
FOR UPDATE;

-- name: SetPlatformNodeBuildConcurrency :execrows
UPDATE eregion_platform_nodes
SET build_concurrency=sqlc.arg(build_concurrency)::integer,updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: ListPlatformNodeWireGuardAddresses :many
SELECT wg_address FROM eregion_platform_nodes WHERE wg_address<>'';

-- name: ListPlatformNodeWireGuardPeers :many
SELECT wg_public_key,wg_address,wg_endpoint
FROM eregion_platform_nodes
WHERE name<>sqlc.arg(exclude_name)::text AND enrollment_state='enrolled'
  AND wg_enabled AND wg_public_key<>'' AND wg_address<>'' AND wg_endpoint<>''
ORDER BY name;

-- name: SetPlatformNodeWireGuardKeys :execrows
UPDATE eregion_platform_nodes
SET wg_public_key=sqlc.arg(wg_public_key)::text, control_wg_public_key=sqlc.arg(control_wg_public_key)::text,
    wg_state=CASE WHEN wg_state IN ('none','failed') THEN 'keyed' ELSE wg_state END,
    updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: SetPlatformNodeWireGuardFailed :execrows
UPDATE eregion_platform_nodes
SET wg_state='failed', updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text AND wg_state<>'applied';

-- name: SetPlatformNodeWireGuardState :execrows
UPDATE eregion_platform_nodes
SET wg_state=sqlc.arg(wg_state)::text, wg_reconciled_at=clock_timestamp(), updated_at=clock_timestamp()
WHERE name=sqlc.arg(name)::text;

-- name: LockPlatformExistingNodes :many
SELECT name FROM eregion_platform_nodes
WHERE name=ANY(sqlc.arg(names)::text[])
ORDER BY name
FOR SHARE;

-- name: GetPlatformNodeDefinitionSnapshot :many
SELECT name,roles,capabilities,status,enrollment_state,transport,last_check_state
FROM eregion_platform_nodes
WHERE name=ANY(sqlc.arg(names)::text[])
ORDER BY name;

-- name: GetPlatformNodeActionFacts :one
SELECT clock_timestamp(),updated_at,roles,capabilities,status,enrollment_state,
       coalesce(enroll_operation_id,''),enrolled_at,identity_fingerprint,last_check_state,last_check_at,wg_state
FROM eregion_platform_nodes WHERE name=sqlc.arg(name)::text;

-- ---------------------------------------------------------------- cross-domain snapshots

-- name: GetPlatformProtectionActionFacts :one
SELECT clock_timestamp(),e.updated_at,e.id,e.protected
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text;

-- name: GetPlatformDeletionCounts :one
SELECT
    (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=sqlc.arg(app)::text AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)),
    (SELECT count(*) FROM eregion_platform_releases r JOIN eregion_platform_apps a ON a.id=r.app_id JOIN eregion_platform_environments e ON e.id=r.environment_id WHERE a.name=sqlc.arg(app)::text AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)),
    (SELECT count(*) FROM eregion_platform_deployments d JOIN eregion_platform_apps a ON a.id=d.app_id JOIN eregion_platform_environments e ON e.id=d.environment_id WHERE a.name=sqlc.arg(app)::text AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)),
    (SELECT count(*) FROM eregion_platform_builds b JOIN eregion_platform_apps a ON a.id=b.app_id JOIN eregion_platform_environments e ON e.id=b.environment_id WHERE a.name=sqlc.arg(app)::text AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)),
    (SELECT count(*) FROM eregion_platform_domains d JOIN eregion_platform_environments e ON e.id=d.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=sqlc.arg(app)::text AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)),
    (SELECT count(*) FROM eregion_secrets s WHERE s.app=sqlc.arg(app)::text AND (sqlc.narg(environment)::text IS NULL OR s.environment=sqlc.narg(environment)::text)),
    (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=sqlc.arg(app)::text);

-- name: GetPlatformWebhookBinding :one
SELECT a.name, e.name, coalesce(e.current_revision_id,''),
       coalesce(r.auto_deploy,false), coalesce(r.webhook_secret_name,'')
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
WHERE e.id=sqlc.arg(environment_id)::text
FOR SHARE OF e;

-- name: GetPlatformPushWebhookBinding :one
SELECT e.id, a.name, e.name, coalesce(e.current_revision_id,''),
       coalesce(r.auto_deploy,false), coalesce(r.webhook_secret_name,''),
       coalesce(r.source,'{}'::jsonb), coalesce(s.state,''), coalesce(s.latest_revision,0),
       coalesce(sr.revision,0), coalesce(sr.key_id,''), sr.ciphertext,
       coalesce(sr.ciphertext_sha256,''), coalesce(sr.revision_hash,''), sr.created_at
FROM eregion_platform_apps a
JOIN eregion_platform_environments e ON e.app_id=a.id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
LEFT JOIN eregion_secrets s ON s.app=a.name AND s.environment=e.name AND s.name=r.webhook_secret_name
LEFT JOIN eregion_secret_revisions sr
  ON sr.app=s.app AND sr.environment=s.environment AND sr.name=s.name AND sr.revision=s.latest_revision
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(environment)::text;

-- name: GetPlatformSecretBindingSnapshot :one
SELECT coalesce(e.current_revision_id,''),
       coalesce(r.auto_deploy,false), coalesce(r.webhook_secret_name,''),
       coalesce(s.state,''), coalesce(s.latest_revision,0),
       coalesce((SELECT sr.revision_hash FROM eregion_secret_revisions sr
                 WHERE sr.app=s.app AND sr.environment=s.environment AND sr.name=s.name
                   AND sr.revision=s.latest_revision), '')
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
LEFT JOIN eregion_secrets s ON s.app=a.name AND s.environment=e.name AND s.name=r.webhook_secret_name
WHERE e.id=sqlc.arg(environment_id)::text;

-- name: GetPlatformDefinitionChildEnvironmentIdentity :one
SELECT a.name,e.name,e.id
FROM eregion_definition_steps s
JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE s.child_operation_id=sqlc.arg(operation_id)::text;

-- name: ListPlatformDomainDNSDefinitionCleanupState :one
SELECT s.dns_progress,s.dns_plan,s.dns_cleanup,s.input,r.approved,coalesce(s.domain_id,''),
       coalesce(x.state,''),coalesce(x.last_error,''),coalesce(x.cleanup_error,''),coalesce(x.last_operation_id,'')
FROM eregion_definition_steps s
JOIN eregion_definition_runs r ON r.operation_id=s.parent_operation_id
LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=s.domain_id
WHERE s.child_operation_id=sqlc.arg(operation_id)::text;

-- name: PlatformDomainDNSDefinitionCleanupPending :one
SELECT EXISTS(
  SELECT 1 FROM eregion_definition_steps s
  WHERE s.parent_operation_id=sqlc.arg(parent_operation_id)::text
    AND s.child_operation_id IS NULL
    AND (s.dns_progress <> '[]'::jsonb OR EXISTS (
      SELECT 1 FROM eregion_operations c JOIN eregion_platform_domain_dns x ON x.last_operation_id=c.id
      WHERE c.idempotency_key='definition:'||s.parent_operation_id||':'||s.ordinal::text
    ) OR EXISTS (
      SELECT 1 FROM eregion_platform_domain_dns x
      WHERE x.domain_id=s.domain_id AND x.state IN ('pending','failed','cleanup_pending','cleanup_failed')
    )));
