-- Operation/domain purpose guards. These are the remaining counts and lists
-- used before admission or settlement. Each statement is one SQL snapshot;
-- lock acquisition and policy remain with the caller.

-- name: CountActiveDomainConflicts :one
SELECT count(*)
FROM eregion_operations
WHERE id <> sqlc.arg(exclude_id)::text
  AND state IN ('queued','running','cancelling')
  AND (
    (kind IN (sqlc.arg(domain_add_kind)::text, sqlc.arg(domain_remove_kind)::text)
      AND target = sqlc.arg(environment_target)::text)
    OR (kind = sqlc.arg(environment_delete_kind)::text
      AND target = sqlc.arg(environment_target)::text)
    OR (kind = sqlc.arg(app_delete_kind)::text
      AND target = sqlc.arg(app_target)::text)
  );

-- name: LatestDomainOperation :one
SELECT id
FROM eregion_operations
WHERE target = sqlc.arg(target)::text
  AND kind IN (sqlc.arg(domain_add_kind)::text, sqlc.arg(domain_remove_kind)::text)
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountActiveDeleteTargetsForApp :one
SELECT count(*)
FROM eregion_operations
WHERE id <> sqlc.arg(exclude_id)::text
  AND state IN ('queued','running','cancelling')
  AND (target = sqlc.arg(app_target)::text
       OR target LIKE sqlc.arg(environment_pattern)::text);

-- name: CountActiveDeleteTargetsForEnvironment :one
SELECT count(*)
FROM eregion_operations
WHERE id <> sqlc.arg(exclude_id)::text
  AND state IN ('queued','running','cancelling')
  AND (target = sqlc.arg(app_target)::text
       OR target = sqlc.arg(environment_target)::text
       OR (kind = sqlc.arg(environment_delete_kind)::text
           AND target LIKE sqlc.arg(environment_pattern)::text));

-- name: CountActiveParentDeletes :one
SELECT count(*)
FROM eregion_operations
WHERE id <> sqlc.arg(exclude_id)::text
  AND state IN ('queued','running','cancelling')
  AND ((kind = sqlc.arg(app_delete_kind)::text AND target = sqlc.arg(app_target)::text)
       OR (kind = sqlc.arg(environment_delete_kind)::text AND target = sqlc.arg(environment_target)::text));

-- name: CountActiveResourceOperations :one
SELECT count(*)
FROM eregion_operations
WHERE id <> sqlc.arg(exclude_id)::text
  AND target = sqlc.arg(target)::text
  AND state IN ('queued','running','cancelling');

-- name: ListActiveAppEnvironmentOperations :many
SELECT kind, state
FROM eregion_operations
WHERE id <> sqlc.arg(exclude_id)::text
  AND target IN (sqlc.arg(environment_target)::text, sqlc.arg(app_target)::text)
  AND state IN ('queued','running','cancelling');

-- name: ListActiveDeploymentsForEnvironment :many
SELECT o.id, o.attempt, o.state, d.id
FROM eregion_operations o
JOIN eregion_platform_deployments d ON d.operation_id = o.id
WHERE d.environment_id = sqlc.arg(environment_id)::text
  AND o.state IN ('queued','running','cancelling')
  AND o.kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote')
ORDER BY o.id;

-- name: CountActiveDeploymentsForEnvironment :one
SELECT count(*)
FROM eregion_operations o
JOIN eregion_platform_deployments d ON d.operation_id = o.id
WHERE d.environment_id = sqlc.arg(environment_id)::text
  AND o.state IN ('queued','running','cancelling')
  AND o.kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote');

-- name: GetPlatformReleaseCommandRunLive :one
SELECT r.state, coalesce(r.operation_id, ''),
       EXISTS(SELECT 1 FROM eregion_operations o
              WHERE o.id = r.operation_id
                AND o.state IN ('queued','running','cancelling'))
FROM eregion_platform_release_command_runs r
WHERE r.release_id = sqlc.arg(release_id)::text
  AND r.command_digest = sqlc.arg(command_digest)::text;

-- name: ListPlatformDefinitionActiveOperations :many
SELECT o.id, o.kind, o.target, o.state
FROM eregion_operations o
WHERE o.state IN ('queued','running','cancelling')
  AND (
    o.target = sqlc.arg(app_target)::text
    OR left(o.target, length(sqlc.arg(env_prefix)::text)) = sqlc.arg(env_prefix)::text
    OR left(o.target, length(sqlc.arg(resource_prefix)::text)) = sqlc.arg(resource_prefix)::text
    OR EXISTS (
      SELECT 1 FROM eregion_platform_builds b
      WHERE o.target = 'build/' || b.id AND b.app_id = sqlc.arg(app_id)::text
    )
  )
ORDER BY o.id;

-- name: GetActiveDeploymentReleaseForEnvironment :one
SELECT d.release_id
FROM eregion_platform_deployments d
JOIN eregion_operations o ON o.id = d.operation_id
WHERE d.environment_id = sqlc.arg(environment_id)::text
  AND d.state IN ('pending','running')
  AND o.state IN ('queued','running','cancelling')
ORDER BY d.created_at, d.id
LIMIT 1;
