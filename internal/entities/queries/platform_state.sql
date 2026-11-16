-- Platform state persistence for the entities foundation. These named
-- statements preserve the existing control-plane SQL: row locks, pointer
-- updates, bounded port allocation, domain binding rules and inventory
-- ordering. They are not called by production yet.

-- ---------------------------------------------------------------- apps

-- name: GetPlatformAppByName :one
SELECT id, name, image_repository, created_at, updated_at
FROM eregion_platform_apps
WHERE name = sqlc.arg(name)::text;

-- name: GetPlatformAppByNameForUpdate :one
SELECT id, name, image_repository, created_at, updated_at
FROM eregion_platform_apps
WHERE name = sqlc.arg(name)::text
FOR UPDATE;

-- name: GetPlatformAppIDByName :one
SELECT id
FROM eregion_platform_apps
WHERE name = sqlc.arg(name)::text;

-- name: GetPlatformAppNameByID :one
SELECT name
FROM eregion_platform_apps
WHERE id = sqlc.arg(id)::text;

-- name: PlatformAppExists :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_apps WHERE name = sqlc.arg(name)::text);

-- name: PlatformAppMissingByID :one
SELECT NOT EXISTS(SELECT 1 FROM eregion_platform_apps WHERE id = sqlc.arg(id)::text);

-- name: GetPlatformAppRepository :one
SELECT id, image_repository
FROM eregion_platform_apps
WHERE name = sqlc.arg(name)::text;

-- name: GetPlatformAppUploadIdentity :one
SELECT id, name, image_repository
FROM eregion_platform_apps
WHERE name = sqlc.arg(name)::text;

-- name: GetPlatformAppActionTimestamps :one
SELECT id, clock_timestamp() AS now, updated_at
FROM eregion_platform_apps
WHERE name = sqlc.arg(name)::text;

-- name: InsertPlatformApp :execrows
INSERT INTO eregion_platform_apps(id, name, image_repository, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(name)::text, sqlc.arg(image_repository)::text,
        clock_timestamp(), clock_timestamp());

-- name: ListPlatformApps :many
SELECT id, name, image_repository, created_at, updated_at
FROM eregion_platform_apps
WHERE (sqlc.narg(name)::text IS NULL OR name = sqlc.narg(name)::text)
ORDER BY name
LIMIT sqlc.arg(max_rows)::integer;

-- name: DeletePlatformApp :execrows
DELETE FROM eregion_platform_apps WHERE id = sqlc.arg(id)::text;

-- ---------------------------------------------------------------- environments

-- name: GetPlatformEnvironmentByName :one
SELECT e.id,a.name,e.name,a.image_repository,e.node_name,e.protected,coalesce(e.build_node,''),e.host_port,e.candidate_port,e.container_port,
    e.bind_address,e.health_type,e.health_path,e.health_timeout_seconds,e.env,e.secret_refs,e.secret_revision,
    coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),coalesce(e.serving_release_id,''),coalesce(e.serving_deployment_id,''),
    e.health_state,e.health_detail,e.health_checked_at,
    coalesce((SELECT array_agg(d.name ORDER BY d.name) FROM eregion_platform_domains d WHERE d.environment_id=e.id),'{}')::text[],
    coalesce((SELECT d.name FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),'')::text,
    coalesce((SELECT d.dns_state FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),'')::text,
    coalesce((SELECT array_agg(c.container_name ORDER BY c.container_name)
              FROM eregion_platform_container_cleanup c WHERE c.environment_id=e.id),'{}')::text[],
    EXISTS(SELECT 1 FROM eregion_platform_releases rr WHERE rr.environment_id=e.id AND rr.status='ready'),
    coalesce(e.current_revision_id,''),coalesce(r.id,''),coalesce(r.number,0),coalesce(r.operation_id,''),
    coalesce(r.actor_kind,'system'),coalesce(r.actor_subject,''),coalesce(r.operation,''),coalesce(r.summary,''),
    coalesce(r.source,'{}'::jsonb),coalesce(r.auto_deploy,false),coalesce(r.webhook_secret_name,''),
    coalesce(r.runtime,'{}'::jsonb),coalesce(r.processes,'[]'::jsonb),coalesce(r.config,'{}'::jsonb),
    coalesce(r.secret_names,'{}'),coalesce(r.resources,'{}'),coalesce(r.retention,'{}'::jsonb),coalesce(r.deploy_intent,false),r.created_at,
    e.created_at,e.updated_at,
    coalesce((e.current_revision_id IS NULL OR r.id IS NOT NULL)
      AND (e.desired_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.desired_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.applied_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.applied_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.serving_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.serving_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.serving_deployment_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_deployments x WHERE x.id=e.serving_deployment_id AND x.environment_id=e.id AND x.app_id=e.app_id AND x.release_id=e.serving_release_id))
    , false)::boolean
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id AND r.environment_id=e.id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(name)::text;

-- name: GetPlatformEnvironmentByID :one
SELECT e.id,a.name,e.name,a.image_repository,e.node_name,e.protected,coalesce(e.build_node,''),e.host_port,e.candidate_port,e.container_port,
    e.bind_address,e.health_type,e.health_path,e.health_timeout_seconds,e.env,e.secret_refs,e.secret_revision,
    coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),coalesce(e.serving_release_id,''),coalesce(e.serving_deployment_id,''),
    e.health_state,e.health_detail,e.health_checked_at,
    coalesce((SELECT array_agg(d.name ORDER BY d.name) FROM eregion_platform_domains d WHERE d.environment_id=e.id),'{}')::text[],
    coalesce((SELECT d.name FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),'')::text,
    coalesce((SELECT d.dns_state FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),'')::text,
    coalesce((SELECT array_agg(c.container_name ORDER BY c.container_name)
              FROM eregion_platform_container_cleanup c WHERE c.environment_id=e.id),'{}')::text[],
    EXISTS(SELECT 1 FROM eregion_platform_releases rr WHERE rr.environment_id=e.id AND rr.status='ready'),
    coalesce(e.current_revision_id,''),coalesce(r.id,''),coalesce(r.number,0),coalesce(r.operation_id,''),
    coalesce(r.actor_kind,'system'),coalesce(r.actor_subject,''),coalesce(r.operation,''),coalesce(r.summary,''),
    coalesce(r.source,'{}'::jsonb),coalesce(r.auto_deploy,false),coalesce(r.webhook_secret_name,''),
    coalesce(r.runtime,'{}'::jsonb),coalesce(r.processes,'[]'::jsonb),coalesce(r.config,'{}'::jsonb),
    coalesce(r.secret_names,'{}'),coalesce(r.resources,'{}'),coalesce(r.retention,'{}'::jsonb),coalesce(r.deploy_intent,false),r.created_at,
    e.created_at,e.updated_at,
    coalesce((e.current_revision_id IS NULL OR r.id IS NOT NULL)
      AND (e.desired_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.desired_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.applied_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.applied_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.serving_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.serving_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.serving_deployment_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_deployments x WHERE x.id=e.serving_deployment_id AND x.environment_id=e.id AND x.app_id=e.app_id AND x.release_id=e.serving_release_id))
    , false)::boolean
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id AND r.environment_id=e.id
WHERE e.id=sqlc.arg(id)::text;

-- name: ListPlatformEnvironments :many
SELECT e.id,a.name,e.name,a.image_repository,e.node_name,e.protected,coalesce(e.build_node,''),e.host_port,e.candidate_port,e.container_port,
    e.bind_address,e.health_type,e.health_path,e.health_timeout_seconds,e.env,e.secret_refs,e.secret_revision,
    coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),coalesce(e.serving_release_id,''),coalesce(e.serving_deployment_id,''),
    e.health_state,e.health_detail,e.health_checked_at,
    coalesce((SELECT array_agg(d.name ORDER BY d.name) FROM eregion_platform_domains d WHERE d.environment_id=e.id),'{}')::text[],
    coalesce((SELECT d.name FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),'')::text,
    coalesce((SELECT d.dns_state FROM eregion_platform_domains d WHERE d.environment_id=e.id AND d.is_primary ORDER BY d.created_at,d.name LIMIT 1),'')::text,
    coalesce((SELECT array_agg(c.container_name ORDER BY c.container_name)
              FROM eregion_platform_container_cleanup c WHERE c.environment_id=e.id),'{}')::text[],
    EXISTS(SELECT 1 FROM eregion_platform_releases rr WHERE rr.environment_id=e.id AND rr.status='ready'),
    coalesce(e.current_revision_id,''),coalesce(r.id,''),coalesce(r.number,0),coalesce(r.operation_id,''),
    coalesce(r.actor_kind,'system'),coalesce(r.actor_subject,''),coalesce(r.operation,''),coalesce(r.summary,''),
    coalesce(r.source,'{}'::jsonb),coalesce(r.auto_deploy,false),coalesce(r.webhook_secret_name,''),
    coalesce(r.runtime,'{}'::jsonb),coalesce(r.processes,'[]'::jsonb),coalesce(r.config,'{}'::jsonb),
    coalesce(r.secret_names,'{}'),coalesce(r.resources,'{}'),coalesce(r.retention,'{}'::jsonb),coalesce(r.deploy_intent,false),r.created_at,
    e.created_at,e.updated_at,
    coalesce((e.current_revision_id IS NULL OR r.id IS NOT NULL)
      AND (e.desired_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.desired_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.applied_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.applied_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.serving_release_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_releases x WHERE x.id=e.serving_release_id AND x.environment_id=e.id AND x.app_id=e.app_id))
      AND (e.serving_deployment_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_deployments x WHERE x.id=e.serving_deployment_id AND x.environment_id=e.id AND x.app_id=e.app_id AND x.release_id=e.serving_release_id))
    , false)::boolean
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id AND r.environment_id=e.id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(node)::text IS NULL OR e.node_name=sqlc.narg(node)::text)
  AND (sqlc.narg(state)::text IS NULL OR e.health_state=sqlc.narg(state)::text)
ORDER BY a.name,e.name
LIMIT sqlc.arg(max_rows)::integer;

-- name: ResolvePlatformEnvironmentID :one
SELECT e.id, e.app_id
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(name)::text;

-- name: GetPlatformEnvironmentAppID :one
SELECT app_id FROM eregion_platform_environments WHERE id=sqlc.arg(id)::text;

-- name: GetPlatformEnvironmentIDByAppName :one
SELECT id FROM eregion_platform_environments
WHERE app_id=sqlc.arg(app_id)::text AND name=sqlc.arg(name)::text;

-- name: LockPlatformEnvironmentByIDAppName :one
SELECT id FROM eregion_platform_environments
WHERE id=sqlc.arg(id)::text AND app_id=sqlc.arg(app_id)::text AND name=sqlc.arg(name)::text
FOR UPDATE;

-- name: LockPlatformEnvironmentByIDApp :one
SELECT id FROM eregion_platform_environments
WHERE id=sqlc.arg(id)::text AND app_id=sqlc.arg(app_id)::text
FOR UPDATE;

-- name: LockPlatformEnvironmentTargetsForApp :many
SELECT e.id
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text
ORDER BY e.id
FOR UPDATE OF e;

-- name: PlatformEnvironmentExists :one
SELECT EXISTS(
  SELECT 1 FROM eregion_platform_environments e
  JOIN eregion_platform_apps a ON a.id=e.app_id
  WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(name)::text);

-- name: PlatformEnvironmentExistsForApp :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_environments WHERE app_id=sqlc.arg(app_id)::text AND id=sqlc.arg(id)::text);

-- name: CountPlatformEnvironmentsForApp :one
SELECT count(*) FROM eregion_platform_environments WHERE app_id=sqlc.arg(app_id)::text;

-- name: GetPlatformEnvironmentCurrentRevision :one
SELECT coalesce(e.current_revision_id,'')
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(name)::text;

-- name: GetPlatformEnvironmentRevisionPointers :one
SELECT coalesce(e.current_revision_id,''), coalesce(d.revision_id,'')
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_deployments d ON d.id=e.serving_deployment_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(name)::text;

-- name: ListPlatformEnvironmentTargets :many
SELECT ('env/'||a.name||'/'||e.name)::text
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE a.name=sqlc.arg(app)::text
ORDER BY e.name;

-- name: ListPlatformEnvironmentRetentionRows :many
SELECT e.id,a.name,e.name,e.node_name,
       coalesce(e.serving_release_id,''),coalesce(e.serving_deployment_id,''),coalesce(r.retention,'{}'::jsonb)
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
WHERE (sqlc.narg(node)::text IS NULL OR e.node_name=sqlc.narg(node)::text)
ORDER BY a.name,e.name;

-- name: GetPlatformEnvironmentRetentionOverride :one
SELECT coalesce(r.retention,'{}'::jsonb)
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
WHERE a.name=sqlc.arg(app)::text AND e.name=sqlc.arg(name)::text;

-- name: InsertPlatformEnvironment :execrows
INSERT INTO eregion_platform_environments
    (id,app_id,name,node_name,build_node,host_port,candidate_port,container_port,bind_address,health_type,health_path,health_timeout_seconds,env,secret_refs,protected,secret_revision,health_state,health_detail,created_at,updated_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(app_id)::text,sqlc.arg(name)::text,sqlc.arg(node_name)::text,
        sqlc.narg(build_node)::text,sqlc.arg(host_port)::integer,sqlc.arg(candidate_port)::integer,
        sqlc.arg(container_port)::integer,sqlc.arg(bind_address)::text,sqlc.arg(health_type)::text,
        sqlc.arg(health_path)::text,sqlc.arg(health_timeout_seconds)::integer,sqlc.arg(env)::jsonb,
        sqlc.arg(secret_refs)::jsonb,sqlc.arg(protected)::boolean,0,'unknown','',clock_timestamp(),clock_timestamp());

-- name: SetPlatformEnvironmentCurrentRevision :execrows
UPDATE eregion_platform_environments
SET current_revision_id=sqlc.arg(revision_id)::text, updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: PublishPlatformEnvironmentMirror :execrows
UPDATE eregion_platform_environments
SET node_name=sqlc.arg(node_name)::text,build_node=sqlc.narg(build_node)::text,
    container_port=sqlc.arg(container_port)::integer,health_type=sqlc.arg(health_type)::text,
    health_path=sqlc.arg(health_path)::text,health_timeout_seconds=sqlc.arg(health_timeout_seconds)::integer,
    env=sqlc.arg(env)::jsonb,secret_refs=sqlc.arg(secret_refs)::jsonb,
    current_revision_id=sqlc.arg(revision_id)::text,updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: SetPlatformEnvironmentProtected :execrows
UPDATE eregion_platform_environments
SET protected=sqlc.arg(protected)::boolean, updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: SetPlatformEnvironmentDesiredRelease :execrows
UPDATE eregion_platform_environments
SET desired_release_id=sqlc.arg(release_id)::text, updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: SetPlatformEnvironmentServing :execrows
UPDATE eregion_platform_environments
SET serving_release_id=NULLIF(sqlc.arg(release_id)::text,''),serving_deployment_id=NULLIF(sqlc.arg(deployment_id)::text,''),
    applied_release_id=NULLIF(sqlc.arg(release_id)::text,''),health_state=sqlc.arg(health_state)::text,
    health_detail=sqlc.arg(health_detail)::text,health_checked_at=sqlc.arg(checked_at)::timestamptz,
    updated_at=sqlc.arg(checked_at)::timestamptz
WHERE id=sqlc.arg(id)::text;

-- name: ClearPlatformEnvironmentPointers :execrows
UPDATE eregion_platform_environments
SET desired_release_id=NULL,applied_release_id=NULL,serving_release_id=NULL,
    serving_deployment_id=NULL,current_revision_id=NULL,updated_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text;

-- name: DeletePlatformEnvironment :execrows
DELETE FROM eregion_platform_environments WHERE id=sqlc.arg(id)::text;

-- name: LockPlatformPortAllocation :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: PlatformPortInUse :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_environments
WHERE host_port=sqlc.arg(port)::integer OR candidate_port=sqlc.arg(port)::integer);

-- name: NextPlatformHostPort :one
SELECT p::integer FROM generate_series(20000,29999) p
WHERE NOT EXISTS (SELECT 1 FROM eregion_platform_environments e WHERE e.host_port=p OR e.candidate_port=p)
ORDER BY p LIMIT 1;

-- name: NextPlatformCandidatePort :one
SELECT p::integer FROM generate_series(30000,39999) p
WHERE p<>sqlc.arg(host_port)::integer
  AND NOT EXISTS (SELECT 1 FROM eregion_platform_environments e WHERE e.candidate_port=p OR e.host_port=p)
ORDER BY p LIMIT 1;

-- ---------------------------------------------------------------- revisions

-- name: GetPlatformRevisionByID :one
SELECT r.id,a.name,e.name,r.number,coalesce(r.operation_id,''),
    r.actor_kind,r.actor_subject,r.operation,r.summary,r.source,r.auto_deploy,r.webhook_secret_name,
    r.runtime,r.processes,r.config,r.secret_names,r.resources,r.retention,r.deploy_intent,r.created_at
FROM eregion_platform_environment_revisions r
JOIN eregion_platform_environments e ON e.id=r.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE r.id=sqlc.arg(id)::text;

-- name: ListPlatformRevisions :many
SELECT r.id,a.name,e.name,r.number,coalesce(r.operation_id,''),
    r.actor_kind,r.actor_subject,r.operation,r.summary,r.source,r.auto_deploy,r.webhook_secret_name,
    r.runtime,r.processes,r.config,r.secret_names,r.resources,r.retention,r.deploy_intent,r.created_at
FROM eregion_platform_environment_revisions r
JOIN eregion_platform_environments e ON e.id=r.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(state)::text IS NULL OR r.operation=sqlc.narg(state)::text)
ORDER BY r.created_at DESC
LIMIT sqlc.arg(max_rows)::integer;

-- name: NextPlatformRevisionNumber :one
SELECT coalesce(max(number),0)+1
FROM eregion_platform_environment_revisions
WHERE environment_id=sqlc.arg(environment_id)::text;

-- name: InsertPlatformRevision :execrows
INSERT INTO eregion_platform_environment_revisions
    (id,environment_id,number,operation_id,actor_kind,actor_subject,operation,summary,
     source,auto_deploy,webhook_secret_name,runtime,processes,config,secret_names,resources,deploy_intent,retention,created_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(environment_id)::text,sqlc.arg(number)::integer,
        sqlc.narg(operation_id)::text,sqlc.arg(actor_kind)::text,sqlc.arg(actor_subject)::text,
        sqlc.arg(operation)::text,sqlc.arg(summary)::text,sqlc.arg(source)::jsonb,
        sqlc.arg(auto_deploy)::boolean,sqlc.arg(webhook_secret_name)::text,
        sqlc.arg(runtime)::jsonb,sqlc.arg(processes)::jsonb,sqlc.arg(config)::jsonb,
        sqlc.arg(secret_names)::text[],sqlc.arg(resources)::text[],sqlc.arg(deploy_intent)::boolean,
        sqlc.arg(retention)::jsonb,clock_timestamp());

-- name: GetPlatformRevisionNumber :one
SELECT number FROM eregion_platform_environment_revisions WHERE id=sqlc.arg(id)::text;

-- name: PlatformRevisionExistsForOperation :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE operation_id=sqlc.arg(operation_id)::text);

-- name: PlatformRevisionUsesResource :one
SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions
WHERE id=sqlc.arg(id)::text AND sqlc.arg(resource)::text=ANY(resources));

-- name: GetPlatformRevisionRuntime :one
SELECT runtime FROM eregion_platform_environment_revisions WHERE id=sqlc.arg(id)::text;

-- name: GetPlatformFirstDeployOperation :one
SELECT o.id
FROM eregion_platform_environment_revisions r
JOIN eregion_operations o ON o.idempotency_key = 'first-deploy:' || r.id
    AND o.kind=sqlc.arg(kind)::text AND o.actor_kind=sqlc.arg(actor_kind)::text AND o.actor_subject=sqlc.arg(actor_subject)::text
WHERE r.operation_id=sqlc.arg(operation_id)::text
ORDER BY r.created_at, o.created_at
LIMIT 1;

-- name: GetPlatformFirstDeployActionRows :many
SELECT r.id,r.environment_id,e.name,a.id,a.name,r.number,r.deploy_intent,
       coalesce((SELECT o.id FROM eregion_operations o
         JOIN eregion_platform_deployments first_dep ON first_dep.operation_id=o.id
         WHERE r.deploy_intent AND o.kind='platform.deploy'
         AND o.idempotency_key='first-deploy:' || r.id
         AND o.actor_kind=source_op.actor_kind AND o.actor_subject=source_op.actor_subject
         AND o.target='env/' || a.name || '/' || e.name
         AND first_dep.revision_id=r.id AND first_dep.environment_id=r.environment_id),''),
       r.processes,r.config,coalesce(p.config,'{}'::jsonb),(r.number=1 OR p.id IS NOT NULL),r.secret_names,r.resources
FROM eregion_platform_environment_revisions r
JOIN eregion_operations source_op ON source_op.id=r.operation_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
JOIN eregion_platform_apps a ON a.id=e.app_id
LEFT JOIN LATERAL (SELECT id,config FROM eregion_platform_environment_revisions prev
  WHERE prev.environment_id=r.environment_id AND prev.number=r.number-1) p ON true
WHERE r.operation_id=sqlc.arg(operation_id)::text
ORDER BY a.name,e.name,r.number;

-- name: DeletePlatformRevisionsForEnvironment :execrows
DELETE FROM eregion_platform_environment_revisions WHERE environment_id=sqlc.arg(environment_id)::text;

-- ---------------------------------------------------------------- changes

-- name: NextPlatformChangeSequenceForApp :one
SELECT coalesce(max(sequence),0)+1
FROM eregion_platform_changes
WHERE app_id=sqlc.arg(app_id)::text AND environment_id IS NULL;

-- name: NextPlatformChangeSequenceForEnvironment :one
SELECT coalesce(max(sequence),0)+1
FROM eregion_platform_changes
WHERE environment_id=sqlc.arg(environment_id)::text;

-- name: InsertPlatformChange :execrows
INSERT INTO eregion_platform_changes
    (id,app_id,environment_id,sequence,kind,operation_id,actor_kind,actor_subject,summary,desired,applied,created_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(app_id)::text,sqlc.narg(environment_id)::text,
        sqlc.arg(sequence)::bigint,sqlc.arg(kind)::text,sqlc.arg(operation_id)::text,
        sqlc.arg(actor_kind)::text,sqlc.arg(actor_subject)::text,sqlc.arg(summary)::text,
        sqlc.narg(desired)::jsonb,sqlc.narg(applied)::jsonb,clock_timestamp())
ON CONFLICT (operation_id,kind) DO NOTHING;

-- name: ListPlatformChanges :many
SELECT c.id,a.name,coalesce(e.name,''),c.sequence,c.kind,c.operation_id,c.actor_kind,c.actor_subject,c.summary,c.desired,c.applied,c.created_at
FROM eregion_platform_changes c
JOIN eregion_platform_apps a ON a.id=c.app_id
LEFT JOIN eregion_platform_environments e ON e.id=c.environment_id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(state)::text IS NULL OR c.kind=sqlc.narg(state)::text)
ORDER BY c.created_at DESC
LIMIT sqlc.arg(max_rows)::integer;

-- name: DeletePlatformChangesForEnvironment :execrows
DELETE FROM eregion_platform_changes WHERE environment_id=sqlc.arg(environment_id)::text;

-- ---------------------------------------------------------------- inventory

-- name: PlatformInventorySummary :many
SELECT 'apps', count(*) FROM eregion_platform_apps
UNION ALL SELECT 'environments', count(*) FROM eregion_platform_environments
UNION ALL SELECT 'releases', count(*) FROM eregion_platform_releases
UNION ALL SELECT 'builds', count(*) FROM eregion_platform_builds
UNION ALL SELECT 'deployments', count(*) FROM eregion_platform_deployments
UNION ALL SELECT 'nodes', count(*) FROM eregion_platform_nodes
UNION ALL SELECT 'resources', count(*) FROM eregion_platform_resources
UNION ALL SELECT 'serving', count(*) FROM eregion_platform_environments WHERE serving_release_id IS NOT NULL;

-- name: PlatformEnvironmentReferencedByAnother :one
SELECT EXISTS (
    SELECT 1 FROM eregion_platform_environments e
    WHERE e.id<>sqlc.arg(id)::text AND (
        e.current_revision_id IN (SELECT id FROM eregion_platform_environment_revisions WHERE environment_id=sqlc.arg(id)::text)
        OR e.desired_release_id IN (SELECT id FROM eregion_platform_releases WHERE environment_id=sqlc.arg(id)::text)
        OR e.applied_release_id IN (SELECT id FROM eregion_platform_releases WHERE environment_id=sqlc.arg(id)::text)
        OR e.serving_release_id IN (SELECT id FROM eregion_platform_releases WHERE environment_id=sqlc.arg(id)::text)
        OR e.serving_deployment_id IN (SELECT id FROM eregion_platform_deployments WHERE environment_id=sqlc.arg(id)::text))
    UNION ALL
    SELECT 1 FROM eregion_platform_releases r
    WHERE r.environment_id<>sqlc.arg(id)::text
      AND r.build_id IN (SELECT id FROM eregion_platform_builds WHERE environment_id=sqlc.arg(id)::text)
);

-- name: PlatformDefinitionReservationExists :one
SELECT EXISTS (
    SELECT 1 FROM eregion_definition_runs r
    JOIN eregion_platform_environments e ON e.id=r.environment_id
    JOIN eregion_platform_apps a ON a.id=e.app_id
    WHERE a.name=sqlc.arg(app)::text AND r.finished_at IS NULL
      AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
      AND NOT EXISTS (
        SELECT 1 FROM eregion_definition_steps st
        JOIN eregion_operations p ON p.id=r.operation_id
        WHERE st.parent_operation_id=r.operation_id AND st.ordinal=r.next_step
          AND st.child_operation_id=sqlc.arg(operation_id)::text
          AND st.kind=sqlc.arg(kind)::text AND st.target=sqlc.arg(target)::text
          AND st.input=sqlc.arg(input)::jsonb AND p.state='running' AND p.kind=sqlc.arg(parent_kind)::text
          AND p.actor_kind=sqlc.arg(actor_kind)::text AND p.actor_subject=sqlc.arg(actor_subject)::text));

-- name: LockPlatformEnvironmentIDForAppName :one
SELECT id FROM eregion_platform_environments
WHERE app_id=sqlc.arg(app_id)::text AND name=sqlc.arg(name)::text
FOR KEY SHARE;


-- name: PlatformEnvironmentHasForeignReferences :one
SELECT EXISTS (
    SELECT 1 FROM eregion_platform_environments e
    WHERE e.id <> sqlc.arg(environment_id)::text AND (
        e.current_revision_id IN (SELECT id FROM eregion_platform_environment_revisions WHERE environment_id = sqlc.arg(environment_id)::text)
        OR e.desired_release_id IN (SELECT id FROM eregion_platform_releases WHERE environment_id = sqlc.arg(environment_id)::text)
        OR e.applied_release_id IN (SELECT id FROM eregion_platform_releases WHERE environment_id = sqlc.arg(environment_id)::text)
        OR e.serving_release_id IN (SELECT id FROM eregion_platform_releases WHERE environment_id = sqlc.arg(environment_id)::text)
        OR e.serving_deployment_id IN (SELECT id FROM eregion_platform_deployments WHERE environment_id = sqlc.arg(environment_id)::text))
    UNION ALL
    SELECT 1 FROM eregion_platform_releases r
    WHERE r.environment_id <> sqlc.arg(environment_id)::text
      AND r.build_id IN (SELECT id FROM eregion_platform_builds WHERE environment_id = sqlc.arg(environment_id)::text));
