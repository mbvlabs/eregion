-- Core operation persistence for the entities foundation. These named
-- statements preserve the existing control-plane SQL: admission conflict
-- handling, identity replay, row locks, attempt claims, state writes and
-- accepted-sequence bookkeeping. They are not called by production yet.

-- name: InsertOperation :execrows
INSERT INTO eregion_operations
    (id, kind, target, actor_kind, actor_subject, idempotency_key, input_hash, input,
     state, attempt, completed_steps, last_sequence, error_code, created_at, updated_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(kind)::text, sqlc.arg(target)::text,
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        sqlc.arg(idempotency_key)::text, sqlc.arg(input_hash)::text,
        sqlc.arg(input)::jsonb, 'queued', 0, 0, 0, '',
        clock_timestamp(), clock_timestamp())
ON CONFLICT (actor_kind, actor_subject, kind, target, idempotency_key) DO NOTHING;

-- name: GetOperationIdentity :one
SELECT id, input_hash
FROM eregion_operations
WHERE actor_kind = sqlc.arg(actor_kind)::text
  AND actor_subject = sqlc.arg(actor_subject)::text
  AND kind = sqlc.arg(kind)::text
  AND target = sqlc.arg(target)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: GetOperationIdentityForShare :one
SELECT id, input_hash
FROM eregion_operations
WHERE actor_kind = sqlc.arg(actor_kind)::text
  AND actor_subject = sqlc.arg(actor_subject)::text
  AND kind = sqlc.arg(kind)::text
  AND target = sqlc.arg(target)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
FOR SHARE;

-- name: GetOperation :one
SELECT id, kind, target, actor_kind, actor_subject, state, attempt,
       last_sequence, error_code, created_at, updated_at
FROM eregion_operations
WHERE id = sqlc.arg(id)::text;
-- name: GetOperationForActor :one
SELECT id, kind, target, actor_kind, actor_subject, state, attempt,
       last_sequence, error_code, created_at, updated_at
FROM eregion_operations
WHERE id = sqlc.arg(id)::text
  AND actor_kind = sqlc.arg(actor_kind)::text
  AND actor_subject = sqlc.arg(actor_subject)::text;

-- name: LockOperation :one
SELECT id, kind, target, actor_kind, actor_subject, state, attempt,
       last_sequence, error_code, created_at, updated_at
FROM eregion_operations
WHERE id = sqlc.arg(id)::text
FOR UPDATE;

-- name: GetOperationStateAttempt :one
SELECT state, attempt
FROM eregion_operations
WHERE id = sqlc.arg(id)::text;

-- name: GetOperationLastSequence :one
SELECT last_sequence
FROM eregion_operations
WHERE id = sqlc.arg(id)::text;

-- name: GetOperationInput :one
SELECT input
FROM eregion_operations
WHERE id = sqlc.arg(id)::text;

-- name: SetOperationJob :exec
UPDATE eregion_operations
SET job_id = sqlc.arg(job_id)::bigint
WHERE id = sqlc.arg(id)::text;

-- name: ClaimOperationAttempt :one
UPDATE eregion_operations
SET attempt = sqlc.arg(attempt)::integer
WHERE id = sqlc.arg(id)::text
RETURNING input;

-- name: SetOperationAttempt :exec
UPDATE eregion_operations
SET attempt = sqlc.arg(attempt)::integer
WHERE id = sqlc.arg(id)::text;

-- name: SetOperationState :exec
UPDATE eregion_operations
SET state = sqlc.arg(state)::text,
    error_code = sqlc.arg(error_code)::text
WHERE id = sqlc.arg(id)::text;

-- name: SetOperationCompletedSteps :exec
UPDATE eregion_operations
SET completed_steps = sqlc.arg(completed_steps)::integer
WHERE id = sqlc.arg(id)::text;

-- name: SetOperationSequence :exec
UPDATE eregion_operations
SET last_sequence = sqlc.arg(last_sequence)::bigint,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE id = sqlc.arg(id)::text;

-- name: ListOperationsForActor :many
SELECT o.id, o.kind, o.target, o.actor_kind, o.actor_subject, o.state, o.attempt,
       o.last_sequence, o.error_code, o.created_at, o.updated_at
FROM eregion_operations o
WHERE o.actor_kind = sqlc.arg(actor_kind)::text
  AND o.actor_subject = sqlc.arg(actor_subject)::text
  AND (sqlc.narg(state)::text IS NULL OR o.state = sqlc.narg(state)::text)
ORDER BY o.created_at DESC, o.id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListOperationsAllActors :many
SELECT o.id, o.kind, o.target, o.actor_kind, o.actor_subject, o.state, o.attempt,
       o.last_sequence, o.error_code, o.created_at, o.updated_at
FROM eregion_operations o
WHERE (sqlc.narg(state)::text IS NULL OR o.state = sqlc.narg(state)::text)
ORDER BY o.created_at DESC, o.id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListOperationsByApp :many
SELECT o.id, o.kind, o.target, o.actor_kind, o.actor_subject, o.state, o.attempt,
       o.last_sequence, o.error_code, o.created_at, o.updated_at
FROM eregion_operations o
WHERE (
        o.target = 'app/' || sqlc.arg(app)::text
        OR o.target LIKE 'env/' || sqlc.arg(app)::text || '/%'
        OR o.target IN (
            SELECT 'build/' || b.id
            FROM eregion_platform_builds b
            JOIN eregion_platform_apps a ON a.id = b.app_id
            WHERE a.name = sqlc.arg(app)::text
        )
      )
  AND (sqlc.narg(state)::text IS NULL OR o.state = sqlc.narg(state)::text)
ORDER BY o.created_at DESC, o.id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListOperationsByAppEnvironment :many
SELECT o.id, o.kind, o.target, o.actor_kind, o.actor_subject, o.state, o.attempt,
       o.last_sequence, o.error_code, o.created_at, o.updated_at
FROM eregion_operations o
WHERE (
        o.target = 'env/' || sqlc.arg(app)::text || '/' || sqlc.arg(environment)::text
        OR o.target IN (
            SELECT 'build/' || b.id
            FROM eregion_platform_builds b
            JOIN eregion_platform_apps a ON a.id = b.app_id
            JOIN eregion_platform_environments e ON e.id = b.environment_id
            WHERE a.name = sqlc.arg(app)::text
              AND e.name = sqlc.arg(environment)::text
        )
      )
  AND (sqlc.narg(state)::text IS NULL OR o.state = sqlc.narg(state)::text)
ORDER BY o.created_at DESC, o.id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListOperationsByIdentityPattern :many
SELECT id, kind, target, actor_kind, actor_subject, state, attempt,
       last_sequence, error_code, created_at, updated_at
FROM eregion_operations
WHERE kind = sqlc.arg(kind)::text
  AND actor_kind = sqlc.arg(actor_kind)::text
  AND actor_subject = sqlc.arg(actor_subject)::text
  AND target = sqlc.arg(target)::text
  AND idempotency_key LIKE sqlc.arg(idempotency_pattern)::text
ORDER BY created_at, id;

-- name: GetOperationJob :one
SELECT job_id
FROM eregion_operations
WHERE id = sqlc.arg(id)::text;
