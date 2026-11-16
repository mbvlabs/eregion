-- Operation step persistence. Steps are durable acknowledgements for node
-- effects: a completed step is never reopened, a failed or running step may be
-- restarted, and acknowledgement counts the completed rows.

-- name: InsertOperationStep :exec
INSERT INTO eregion_operation_steps
    (operation_id, step, inputs_hash, state, attempt, execution_id, started_at, error, error_code)
VALUES (sqlc.arg(operation_id)::text, sqlc.arg(step)::text, sqlc.arg(inputs_hash)::text,
        'running', sqlc.arg(attempt)::integer, sqlc.arg(execution_id)::text,
        clock_timestamp(), '', '');

-- name: GetOperationStep :one
SELECT operation_id, step, inputs_hash, state, attempt, execution_id,
       started_at, completed_at, error, error_code, result
FROM eregion_operation_steps
WHERE operation_id = sqlc.arg(operation_id)::text
  AND step = sqlc.arg(step)::text;

-- name: LockOperationStep :one
SELECT operation_id, step, inputs_hash, state, attempt, execution_id,
       started_at, completed_at, error, error_code, result
FROM eregion_operation_steps
WHERE operation_id = sqlc.arg(operation_id)::text
  AND step = sqlc.arg(step)::text
FOR UPDATE;

-- name: ListOperationSteps :many
SELECT operation_id, step, inputs_hash, state, attempt, execution_id,
       started_at, completed_at, error, error_code, result
FROM eregion_operation_steps
WHERE operation_id = sqlc.arg(operation_id)::text
ORDER BY step;

-- name: ListOperationStepsByPattern :many
SELECT operation_id, step, inputs_hash, state, attempt, execution_id,
       started_at, completed_at, error, error_code, result
FROM eregion_operation_steps
WHERE operation_id = sqlc.arg(operation_id)::text
  AND (
        step LIKE ANY(sqlc.arg(patterns)::text[])
        OR step = ANY(sqlc.arg(exact)::text[])
      )
ORDER BY step;

-- name: RestartOperationStep :execrows
UPDATE eregion_operation_steps
SET state = 'running',
    attempt = sqlc.arg(attempt)::integer,
    execution_id = sqlc.arg(execution_id)::text,
    started_at = clock_timestamp(),
    error = ''
WHERE operation_id = sqlc.arg(operation_id)::text
  AND step = sqlc.arg(step)::text;

-- name: CompleteOperationStep :execrows
UPDATE eregion_operation_steps
SET state = 'completed',
    completed_at = clock_timestamp(),
    error = '',
    result = sqlc.narg(result)::jsonb,
    execution_id = sqlc.arg(execution_id)::text
WHERE operation_id = sqlc.arg(operation_id)::text
  AND step = sqlc.arg(step)::text;

-- name: FailOperationStep :execrows
UPDATE eregion_operation_steps
SET state = 'failed',
    completed_at = clock_timestamp(),
    error = sqlc.arg(message)::text,
    error_code = sqlc.arg(error_code)::text
WHERE operation_id = sqlc.arg(operation_id)::text
  AND step = sqlc.arg(step)::text
  AND state <> 'completed';

-- name: FailIncompleteOperationSteps :execrows
UPDATE eregion_operation_steps
SET state = 'failed',
    completed_at = clock_timestamp(),
    error = sqlc.arg(message)::text,
    error_code = sqlc.arg(error_code)::text
WHERE operation_id = sqlc.arg(operation_id)::text
  AND state <> 'completed';

-- name: CountCompletedOperationSteps :one
SELECT count(*)
FROM eregion_operation_steps
WHERE operation_id = sqlc.arg(operation_id)::text
  AND state = 'completed';
