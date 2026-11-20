-- Platform release, build and deployment persistence for the entities
-- foundation. Existing select fragments, row locks and state fences are
-- preserved exactly.

-- ---------------------------------------------------------------- releases

-- name: GetPlatformReleaseByID :one
SELECT r.id,r.operation_id,a.name,e.name,r.image,r.image_id,r.source,r.build,r.command,r.status,
    r.error_code,r.error,r.created_at,r.completed_at,coalesce(r.build_id,''),r.commit_sha,r.pruned_at,
    (r.build_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_builds b WHERE b.id=r.build_id AND b.app_id=r.app_id AND b.environment_id=r.environment_id AND b.release_id=r.id))
FROM eregion_platform_releases r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE r.id=sqlc.arg(id)::text;

-- name: ListPlatformReleases :many
SELECT r.id,r.operation_id,a.name,e.name,r.image,r.image_id,r.source,r.build,r.command,r.status,
    r.error_code,r.error,r.created_at,r.completed_at,coalesce(r.build_id,''),r.commit_sha,r.pruned_at,
    (r.build_id IS NULL OR EXISTS (SELECT 1 FROM eregion_platform_builds b WHERE b.id=r.build_id AND b.app_id=r.app_id AND b.environment_id=r.environment_id AND b.release_id=r.id))
FROM eregion_platform_releases r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(state)::text IS NULL OR r.status=sqlc.narg(state)::text)
ORDER BY r.created_at DESC
LIMIT sqlc.arg(max_rows)::integer;

-- name: GetPlatformReleaseIDForOperation :one
SELECT id FROM eregion_platform_releases WHERE operation_id=sqlc.arg(operation_id)::text;

-- name: InsertPlatformRelease :execrows
INSERT INTO eregion_platform_releases
    (id,operation_id,app_id,environment_id,image,image_id,source,build,command,status,commit_sha,
     error_code,error,created_at,completed_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(operation_id)::text,sqlc.arg(app_id)::text,
        sqlc.arg(environment_id)::text,sqlc.arg(image)::text,sqlc.arg(image_id)::text,
        sqlc.arg(source)::jsonb,sqlc.narg(build)::jsonb,sqlc.narg(command)::jsonb,
        sqlc.arg(status)::text,sqlc.arg(commit_sha)::text,sqlc.arg(error_code)::text,
        sqlc.arg(error)::text,clock_timestamp(),sqlc.narg(completed_at)::timestamptz);

-- name: MarkPlatformReleaseReadyWithImageID :execrows
UPDATE eregion_platform_releases
SET status='ready',image_id=sqlc.arg(image_id)::text,completed_at=clock_timestamp(),error='',error_code=''
WHERE id=sqlc.arg(id)::text;

-- name: MarkPlatformOwnedReleaseReadyWithImageID :execrows
UPDATE eregion_platform_releases
SET status='ready',image_id=sqlc.arg(image_id)::text,completed_at=clock_timestamp(),error='',error_code=''
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text AND status IN ('pending','ready')
  AND image=sqlc.arg(image)::text AND image_id=sqlc.arg(image_id)::text;

-- name: MarkPlatformReleaseReadyFromBuild :execrows
UPDATE eregion_platform_releases
SET status='ready',image=sqlc.arg(image)::text,image_id=sqlc.arg(image_id)::text,
    commit_sha=sqlc.arg(commit_sha)::text,completed_at=clock_timestamp(),error='',error_code=''
WHERE id=sqlc.arg(id)::text AND status='pending';

-- name: MarkPlatformReleaseFailedByID :execrows
UPDATE eregion_platform_releases
SET status='failed',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND status='pending';

-- name: MarkPlatformReleaseFailedByOperation :execrows
UPDATE eregion_platform_releases
SET status='failed',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND status='pending';

-- name: MarkPlatformReleasePruned :execrows
UPDATE eregion_platform_releases
SET pruned_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND pruned_at IS NULL;

-- name: GetPlatformReleaseCommand :one
SELECT command FROM eregion_platform_releases WHERE id=sqlc.arg(id)::text;

-- name: LockPlatformReleaseServingEnvironment :one
SELECT e.id,coalesce(e.serving_release_id,''),coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,'')
FROM eregion_platform_releases r
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE r.id=sqlc.arg(id)::text
FOR UPDATE OF e;

-- name: PlatformReleaseImageStillShared :one
SELECT r.id
FROM eregion_platform_releases r
WHERE r.image_id=sqlc.arg(image_id)::text AND r.id<>sqlc.arg(exclude_id)::text AND (
    r.pruned_at IS NULL
    OR EXISTS (SELECT 1 FROM eregion_platform_deployments d
               WHERE d.state IN ('pending','running') AND (d.release_id=r.id OR d.previous_release_id=r.id))
    OR EXISTS (SELECT 1 FROM eregion_operations o
               WHERE o.id=r.operation_id AND o.state IN ('queued','running','cancelling'))
)
ORDER BY r.created_at DESC,r.id
LIMIT 1;

-- name: ListPlatformReleaseRetentionRows :many
SELECT r.id,a.name,e.name,r.image,coalesce(r.image_id,''),r.status,r.created_at,r.pruned_at
FROM eregion_platform_releases r
JOIN eregion_platform_apps a ON a.id=r.app_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE (sqlc.narg(node)::text IS NULL OR e.node_name=sqlc.narg(node)::text)
ORDER BY r.created_at,r.id;

-- name: ListPlatformActiveReleaseIDsForNode :many
SELECT r.id
FROM eregion_platform_releases r
JOIN eregion_operations o ON o.id=r.operation_id
JOIN eregion_platform_environments e ON e.id=r.environment_id
WHERE (sqlc.narg(node)::text IS NULL OR e.node_name=sqlc.narg(node)::text)
  AND o.state IN ('queued','running','cancelling');

-- name: CountPlatformActiveDeploymentsForRelease :one
SELECT count(*)
FROM eregion_platform_deployments
WHERE state IN ('pending','running')
  AND (release_id=sqlc.arg(release_id)::text OR previous_release_id=sqlc.arg(release_id)::text);

-- name: CountPlatformActiveOperationsForRelease :one
SELECT count(*)
FROM eregion_operations o
JOIN eregion_platform_releases r ON r.operation_id=o.id
WHERE r.id=sqlc.arg(release_id)::text AND o.state IN ('queued','running','cancelling');

-- name: DeletePlatformReleasesForEnvironment :execrows
DELETE FROM eregion_platform_releases WHERE environment_id=sqlc.arg(environment_id)::text;

-- ---------------------------------------------------------------- builds

-- name: GetPlatformBuildByID :one
SELECT b.id,b.operation_id,a.name,e.name,coalesce(b.revision_id,''),coalesce(b.release_id,''),
    b.source,b.commit_sha,b.state,b.error_code,b.error,b.created_at,b.started_at,b.completed_at
FROM eregion_platform_builds b
JOIN eregion_platform_apps a ON a.id=b.app_id
JOIN eregion_platform_environments e ON e.id=b.environment_id
WHERE b.id=sqlc.arg(id)::text;

-- name: GetPlatformBuildByOperation :one
SELECT b.id,b.operation_id,a.name,e.name,coalesce(b.revision_id,''),coalesce(b.release_id,''),
    b.source,b.commit_sha,b.state,b.error_code,b.error,b.created_at,b.started_at,b.completed_at
FROM eregion_platform_builds b
JOIN eregion_platform_apps a ON a.id=b.app_id
JOIN eregion_platform_environments e ON e.id=b.environment_id
WHERE b.operation_id=sqlc.arg(operation_id)::text;

-- name: ListPlatformBuilds :many
SELECT b.id,b.operation_id,a.name,e.name,coalesce(b.revision_id,''),coalesce(b.release_id,''),
    b.source,b.commit_sha,b.state,b.error_code,b.error,b.created_at,b.started_at,b.completed_at
FROM eregion_platform_builds b
JOIN eregion_platform_apps a ON a.id=b.app_id
JOIN eregion_platform_environments e ON e.id=b.environment_id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(state)::text IS NULL OR b.state=sqlc.narg(state)::text)
  AND (sqlc.narg(build_id)::text IS NULL OR b.id=sqlc.narg(build_id)::text)
ORDER BY b.created_at DESC
LIMIT sqlc.arg(max_rows)::integer;

-- name: InsertPlatformBuild :execrows
INSERT INTO eregion_platform_builds
    (id,operation_id,app_id,environment_id,revision_id,release_id,source,commit_sha,state,error_code,error,created_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(operation_id)::text,sqlc.arg(app_id)::text,
        sqlc.arg(environment_id)::text,sqlc.narg(revision_id)::text,sqlc.narg(release_id)::text,
        sqlc.arg(source)::jsonb,sqlc.arg(commit_sha)::text,'pending','','',clock_timestamp());

-- name: GetPlatformBuildPlacement :one
SELECT a.name,e.name
FROM eregion_platform_builds b
JOIN eregion_platform_apps a ON a.id=b.app_id
JOIN eregion_platform_environments e ON e.id=b.environment_id
WHERE b.id=sqlc.arg(id)::text;

-- name: GetPlatformBuildState :one
SELECT state FROM eregion_platform_builds WHERE id=sqlc.arg(id)::text;

-- name: PlatformBuildActiveOperationForApp :one
SELECT EXISTS (
  SELECT 1 FROM eregion_operations o
  JOIN eregion_platform_builds b ON o.target='build/'||b.id
  WHERE b.app_id=sqlc.arg(app_id)::text
    AND o.state IN ('queued','running','cancelling'));

-- name: ListPlatformBuildActiveTargetsForApp :many
SELECT o.id,o.kind,o.target,o.state
FROM eregion_operations o
WHERE o.state IN ('queued','running','cancelling') AND (
  o.target=sqlc.arg(target)::text
  OR left(o.target,length(sqlc.arg(app_target_prefix)::text))=sqlc.arg(app_target_prefix)::text
  OR left(o.target,length(sqlc.arg(env_target_prefix)::text))=sqlc.arg(env_target_prefix)::text
  OR EXISTS (SELECT 1 FROM eregion_platform_builds b WHERE o.target='build/'||b.id AND b.app_id=sqlc.arg(app_id)::text)
)
ORDER BY o.id;

-- name: MarkPlatformBuildRunningForOperation :execrows
UPDATE eregion_platform_builds
SET state='running',started_at=coalesce(started_at,clock_timestamp()),error='',error_code=''
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text
  AND release_id=sqlc.arg(release_id)::text AND state IN ('pending','running');

-- name: MarkPlatformBuildSucceeded :execrows
UPDATE eregion_platform_builds
SET state='succeeded',commit_sha=sqlc.arg(commit_sha)::text,completed_at=clock_timestamp(),error='',error_code=''
WHERE id=sqlc.arg(id)::text AND state IN ('pending','running');

-- name: MarkPlatformBuildFailedByID :execrows
UPDATE eregion_platform_builds
SET state='failed',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND state NOT IN ('succeeded','cancelled');

-- name: MarkPlatformBuildCancelledByID :execrows
UPDATE eregion_platform_builds
SET state='cancelled',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND state NOT IN ('succeeded','failed');

-- name: CancelPendingPlatformBuild :execrows
UPDATE eregion_platform_builds
SET state='cancelled',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE id=sqlc.arg(id)::text AND operation_id=sqlc.arg(operation_id)::text AND state='pending';

-- name: MarkPlatformBuildFailedByOperation :execrows
UPDATE eregion_platform_builds
SET state='failed',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('pending','running');

-- name: MarkPlatformBuildCancelledByOperation :execrows
UPDATE eregion_platform_builds
SET state='cancelled',error_code=sqlc.arg(error_code)::text,error=sqlc.arg(message)::text,
    completed_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('pending','running');

-- name: GetPlatformBuildActionFacts :one
SELECT clock_timestamp(),id,operation_id,state,commit_sha,coalesce(revision_id,''),
       coalesce(release_id,''),completed_at
FROM eregion_platform_builds WHERE id=sqlc.arg(id)::text;

-- name: DeletePlatformBuildsForEnvironment :execrows
DELETE FROM eregion_platform_builds WHERE environment_id=sqlc.arg(environment_id)::text;

-- ---------------------------------------------------------------- deployments

-- name: GetPlatformDeploymentByID :one
SELECT d.id,d.operation_id,a.name,e.name,d.action,d.release_id,
    coalesce(d.previous_release_id,''),d.state,d.error_code,d.message,d.created_at,d.updated_at,d.completed_at,
    coalesce(d.revision_id,''),d.secret_revisions,coalesce(d.previous_deployment_id,''),d.instances
FROM eregion_platform_deployments d
JOIN eregion_platform_apps a ON a.id=d.app_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
WHERE d.id=sqlc.arg(id)::text;

-- name: GetPlatformDeploymentByOperation :one
SELECT d.id,d.operation_id,a.name,e.name,d.action,d.release_id,
    coalesce(d.previous_release_id,''),d.state,d.error_code,d.message,d.created_at,d.updated_at,d.completed_at,
    coalesce(d.revision_id,''),d.secret_revisions,coalesce(d.previous_deployment_id,''),d.instances
FROM eregion_platform_deployments d
JOIN eregion_platform_apps a ON a.id=d.app_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
WHERE d.operation_id=sqlc.arg(operation_id)::text;

-- name: ListPlatformDeployments :many
SELECT d.id,d.operation_id,a.name,e.name,d.action,d.release_id,
    coalesce(d.previous_release_id,''),d.state,d.error_code,d.message,d.created_at,d.updated_at,d.completed_at,
    coalesce(d.revision_id,''),d.secret_revisions,coalesce(d.previous_deployment_id,''),d.instances
FROM eregion_platform_deployments d
JOIN eregion_platform_apps a ON a.id=d.app_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
WHERE (sqlc.narg(app)::text IS NULL OR a.name=sqlc.narg(app)::text)
  AND (sqlc.narg(environment)::text IS NULL OR e.name=sqlc.narg(environment)::text)
  AND (sqlc.narg(state)::text IS NULL OR d.state=sqlc.narg(state)::text)
  AND (sqlc.narg(deployment_id)::text IS NULL OR d.id=sqlc.narg(deployment_id)::text)
ORDER BY d.created_at DESC
LIMIT sqlc.arg(max_rows)::integer;

-- name: InsertPlatformDeployment :execrows
INSERT INTO eregion_platform_deployments
    (id,operation_id,app_id,environment_id,action,release_id,previous_release_id,previous_deployment_id,revision_id,secret_revisions,state,error_code,message,instances,created_at,updated_at)
VALUES (sqlc.arg(id)::text,sqlc.arg(operation_id)::text,sqlc.arg(app_id)::text,
        sqlc.arg(environment_id)::text,sqlc.arg(action)::text,sqlc.arg(release_id)::text,
        sqlc.narg(previous_release_id)::text,sqlc.narg(previous_deployment_id)::text,
        sqlc.narg(revision_id)::text,sqlc.arg(secret_revisions)::jsonb,'pending','','','[]'::jsonb,
        clock_timestamp(),clock_timestamp());

-- name: MarkPlatformDeploymentRunningByOperation :execrows
UPDATE eregion_platform_deployments
SET state='running', updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('pending','running');

-- name: MarkPlatformDeploymentSucceededByOperation :execrows
UPDATE eregion_platform_deployments
SET state='succeeded',completed_at=sqlc.arg(at)::timestamptz,updated_at=sqlc.arg(at)::timestamptz,
    message=sqlc.arg(message)::text,error_code=''
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('pending','running');

-- name: MarkPlatformDeploymentFailedByOperation :execrows
UPDATE eregion_platform_deployments
SET state='failed',error_code=sqlc.arg(error_code)::text,message=sqlc.arg(message)::text,
    completed_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('pending','running');

-- name: MarkPlatformDeploymentCancelledByOperation :execrows
UPDATE eregion_platform_deployments
SET state='cancelled',message=sqlc.arg(message)::text,completed_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND state IN ('pending','running');

-- name: SetPlatformDeploymentInstances :execrows
UPDATE eregion_platform_deployments
SET instances=sqlc.arg(instances)::jsonb, updated_at=sqlc.arg(at)::timestamptz
WHERE id=sqlc.arg(id)::text;

-- name: SetPlatformDeploymentInstancesByOperation :execrows
UPDATE eregion_platform_deployments
SET instances=sqlc.arg(instances)::jsonb, updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text;

-- name: GetPlatformDeploymentPredecessor :one
SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'')
FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text;

-- name: BindPlatformDeploymentPredecessor :execrows
UPDATE eregion_platform_deployments
SET previous_release_id=sqlc.narg(previous_release_id)::text,
    previous_deployment_id=sqlc.narg(previous_deployment_id)::text,
    updated_at=updated_at
WHERE id=sqlc.arg(id)::text;

-- name: SetPlatformDeploymentRevisionAndSecretSnapshot :execrows
UPDATE eregion_platform_deployments
SET revision_id=coalesce(revision_id,sqlc.narg(revision_id)::text),
    secret_revisions=CASE WHEN secret_revisions='{}'::jsonb THEN sqlc.arg(secret_revisions)::jsonb ELSE secret_revisions END,
    updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text;

-- name: PinPlatformDeploymentCredentialSecret :execrows
UPDATE eregion_platform_deployments
SET secret_revisions=secret_revisions || jsonb_build_object(sqlc.arg(name)::text,sqlc.arg(ref)::jsonb),
    updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id)::text AND secret_revisions<>'{}'::jsonb
  AND NOT (secret_revisions ? sqlc.arg(name)::text);

-- name: GetPlatformRollbackSnapshot :one
SELECT coalesce(revision_id,''),secret_revisions
FROM eregion_platform_deployments
WHERE environment_id=sqlc.arg(environment_id)::text AND release_id=sqlc.arg(release_id)::text
  AND state='succeeded'
ORDER BY completed_at DESC NULLS LAST, created_at DESC
LIMIT 1;

-- name: GetPlatformPreviousReleaseOfServing :one
SELECT coalesce(previous_release_id,'')
FROM eregion_platform_deployments
WHERE environment_id=sqlc.arg(environment_id)::text AND state='succeeded'
  AND release_id=sqlc.arg(release_id)::text AND previous_release_id IS NOT NULL
ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC
LIMIT 1;

-- name: GetPlatformLatestSucceededReleaseExcluding :one
SELECT release_id
FROM eregion_platform_deployments
WHERE environment_id=sqlc.arg(environment_id)::text AND state='succeeded'
  AND release_id IS DISTINCT FROM sqlc.narg(release_id)::text
ORDER BY completed_at DESC NULLS LAST, created_at DESC, id DESC
LIMIT 1;

-- name: GetPlatformActiveDeploymentReleaseForEnvironment :one
SELECT d.release_id
FROM eregion_platform_deployments d
JOIN eregion_operations o ON o.id=d.operation_id
WHERE d.environment_id=sqlc.arg(environment_id)::text
  AND d.state IN ('pending','running')
  AND o.state IN ('queued','running','cancelling')
ORDER BY d.created_at, d.id
LIMIT 1;

-- name: ListPlatformDeploymentResourceDependencies :many
SELECT coalesce(r.id,''), coalesce(r.resources,'{}'::text[]), coalesce(pr.resources,'{}'::text[])
FROM eregion_platform_deployments d
LEFT JOIN eregion_platform_environment_revisions r ON r.id=d.revision_id
LEFT JOIN eregion_platform_deployments p ON p.id=d.previous_deployment_id
LEFT JOIN eregion_platform_environment_revisions pr ON pr.id=p.revision_id
WHERE d.environment_id=sqlc.arg(environment_id)::text AND d.state IN ('pending','running');

-- name: GetPlatformDeploymentReleaseAndServing :one
SELECT coalesce(d.release_id,''),coalesce(e.serving_release_id,'')
FROM eregion_platform_deployments d
JOIN eregion_platform_environments e ON e.id=d.environment_id
WHERE d.operation_id=sqlc.arg(operation_id)::text;

-- name: GetPlatformDeploymentFailureFacts :one
SELECT d.action,a.name,e.name,d.release_id,coalesce(d.previous_release_id,''),coalesce(e.serving_release_id,'')
FROM eregion_platform_deployments d
JOIN eregion_platform_apps a ON a.id=d.app_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
WHERE d.operation_id=sqlc.arg(operation_id)::text;

-- name: GetPlatformDeploymentExecutionFacts :one
SELECT coalesce(d.previous_release_id,''),coalesce(d.previous_deployment_id,''),
       clock_timestamp(),e.updated_at,coalesce(e.serving_deployment_id,''),coalesce(e.serving_release_id,''),
       coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),e.health_state,e.health_checked_at,
       coalesce(o.operation_id,''),coalesce(o.deployment_id,''),coalesce(o.phase,''),coalesce(o.route_target_deployment_id,''),
       (SELECT count(*) FROM eregion_platform_container_cleanup c WHERE c.deployment_id=d.id),
       (SELECT count(*) FROM eregion_platform_container_cleanup c WHERE c.deployment_id=d.previous_deployment_id)
FROM eregion_platform_deployments d
JOIN eregion_platform_environments e ON e.id=d.environment_id
LEFT JOIN eregion_platform_deployment_owners o ON o.environment_id=e.id
WHERE d.id=sqlc.arg(id)::text AND d.operation_id=sqlc.arg(operation_id)::text;

-- name: ListPlatformDeploymentRetentionRows :many
SELECT d.id,a.name,e.name,coalesce(d.release_id,''),coalesce(d.previous_release_id,''),d.state,d.created_at,d.completed_at
FROM eregion_platform_deployments d
JOIN eregion_platform_apps a ON a.id=d.app_id
JOIN eregion_platform_environments e ON e.id=d.environment_id
WHERE (sqlc.narg(node)::text IS NULL OR e.node_name=sqlc.narg(node)::text)
  AND d.state IN ('succeeded','pending','running')
ORDER BY d.created_at,d.id;

-- name: ListPlatformDeploymentWorkloadNodes :many
SELECT DISTINCT coalesce(r.runtime->>'node','')::text
FROM eregion_platform_deployments d
LEFT JOIN eregion_platform_environment_revisions r ON r.id=d.revision_id
WHERE d.environment_id=sqlc.arg(environment_id)::text;

-- name: ListPlatformRepairTargets :many
SELECT a.name,e.name,coalesce(e.serving_deployment_id,''),
       coalesce(dep.instances,'[]'::jsonb),
       coalesce(NULLIF(r.runtime->>'node',''),e.node_name)
FROM eregion_platform_environments e
JOIN eregion_platform_apps a ON a.id=e.app_id
JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id AND dep.state='succeeded'
LEFT JOIN eregion_platform_environment_revisions r ON r.id=dep.revision_id
WHERE e.serving_deployment_id IS NOT NULL
ORDER BY a.name,e.name;

-- name: DeletePlatformDeploymentsForEnvironment :execrows
DELETE FROM eregion_platform_deployments WHERE environment_id=sqlc.arg(environment_id)::text;

-- name: GetPlatformBuildDeploymentFacts :one
SELECT coalesce(b.id,''),coalesce(b.state,''),coalesce(b.commit_sha,''),
       coalesce(d.id,''),coalesce(d.state,''),coalesce(d.revision_id,b.revision_id,''),
       coalesce(d.release_id,b.release_id,r.id,''),coalesce(r.image,''),coalesce(r.image_id,'')
FROM (SELECT sqlc.arg(operation_id)::text AS operation_id) o
LEFT JOIN eregion_platform_builds b ON b.operation_id=o.operation_id
LEFT JOIN eregion_platform_deployments d ON d.operation_id=o.operation_id
LEFT JOIN eregion_platform_releases r ON r.id=coalesce(d.release_id,b.release_id)
  OR (d.id IS NULL AND b.id IS NULL AND r.operation_id=o.operation_id)
LIMIT 1;

-- name: GetPlatformResultIdentity :one
SELECT
    coalesce((SELECT id FROM eregion_platform_builds WHERE operation_id=sqlc.arg(operation_id)::text),''),
    coalesce((SELECT id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text),''),
    coalesce((SELECT revision_id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text),
             (SELECT revision_id FROM eregion_platform_builds WHERE operation_id=sqlc.arg(operation_id)::text),
             (SELECT id FROM eregion_platform_environment_revisions WHERE operation_id=sqlc.arg(operation_id)::text ORDER BY created_at LIMIT 1),''),
    coalesce((SELECT release_id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text),
             (SELECT id FROM eregion_platform_releases WHERE operation_id=sqlc.arg(operation_id)::text),''),
    coalesce((SELECT image FROM eregion_platform_releases WHERE id=coalesce(
             (SELECT release_id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text),
             (SELECT id FROM eregion_platform_releases WHERE operation_id=sqlc.arg(operation_id)::text))),''),
    coalesce((SELECT image_id FROM eregion_platform_releases WHERE id=coalesce(
             (SELECT release_id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text),
             (SELECT id FROM eregion_platform_releases WHERE operation_id=sqlc.arg(operation_id)::text))),''),
    coalesce((SELECT commit_sha FROM eregion_platform_releases WHERE id=coalesce(
             (SELECT release_id FROM eregion_platform_deployments WHERE operation_id=sqlc.arg(operation_id)::text),
             (SELECT id FROM eregion_platform_releases WHERE operation_id=sqlc.arg(operation_id)::text))),
             (SELECT commit_sha FROM eregion_platform_builds WHERE operation_id=sqlc.arg(operation_id)::text),'');
