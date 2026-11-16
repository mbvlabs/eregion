-- Workspace identity persistence. These statements are the identity read,
-- one-time insert, schema-existence observation and assignment advisory lock.
-- Assignment policy and readiness stay in the control plane; no raw query or
-- transaction crosses this boundary.

-- name: GetWorkspaceIdentity :one
SELECT workspace_id, name, registry_host_node, registry_host_key_type,
       registry_host_key_fingerprint, registry_host_state_dir
FROM eregion_workspace
WHERE singleton;

-- name: WorkspaceSchemaExists :one
SELECT (to_regclass('eregion_workspace') IS NOT NULL)::boolean;

-- name: LockWorkspaceAssignment :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: InsertWorkspaceIdentity :execrows
INSERT INTO eregion_workspace
    (singleton, workspace_id, name, registry_host_node, registry_host_key_type,
     registry_host_key_fingerprint, registry_host_state_dir, created_at)
VALUES (true, sqlc.arg(workspace_id)::text, sqlc.arg(name)::text,
        sqlc.narg(registry_host_node)::text, sqlc.narg(registry_host_key_type)::text,
        sqlc.narg(registry_host_key_fingerprint)::text, sqlc.narg(registry_host_state_dir)::text,
        clock_timestamp())
ON CONFLICT (singleton) DO NOTHING;
