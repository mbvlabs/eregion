-- Definition run journal. Statement text preserves the existing control-plane
-- SQL: active-run reservation is the partial unique index, the run cursor is
-- advanced only by compare-and-set, and the journal lock is FOR UPDATE OF the
-- run while the operation is joined for identity comparison.

-- name: InsertDefinitionRun :exec
INSERT INTO eregion_definition_runs
    (operation_id, plan_nonce, environment_id, approved, next_step, failure_code, failure_message)
VALUES (sqlc.arg(operation_id)::text, sqlc.arg(plan_nonce)::text,
        sqlc.arg(environment_id)::text, sqlc.arg(approved)::jsonb,
        sqlc.arg(next_step)::integer, sqlc.arg(failure_code)::text,
        sqlc.arg(failure_message)::text);

-- name: GetDefinitionRun :one
SELECT operation_id, plan_nonce, environment_id, approved, next_step,
       failure_code, failure_message, finished_at
FROM eregion_definition_runs
WHERE operation_id = sqlc.arg(operation_id)::text;

-- name: LockDefinitionRun :one
SELECT operation_id, plan_nonce, environment_id, approved, next_step,
       failure_code, failure_message, finished_at
FROM eregion_definition_runs
WHERE operation_id = sqlc.arg(operation_id)::text
FOR UPDATE;

-- name: LockDefinitionRunJournal :one
SELECT r.approved, r.environment_id, r.next_step, r.failure_code, r.failure_message,
       coalesce(o.input = r.approved->'input', false)::boolean AS same_input,
       (r.finished_at IS NOT NULL)::boolean AS finished
FROM eregion_definition_runs r
JOIN eregion_operations o ON o.id = r.operation_id
WHERE r.operation_id = sqlc.arg(operation_id)::text
FOR UPDATE OF r;

-- name: SetDefinitionRunFailure :execrows
UPDATE eregion_definition_runs
SET failure_code = sqlc.arg(failure_code)::text,
    failure_message = sqlc.arg(failure_message)::text
WHERE operation_id = sqlc.arg(operation_id)::text
  AND failure_code = ''
  AND finished_at IS NULL;

-- name: FinishDefinitionRun :execrows
UPDATE eregion_definition_runs
SET finished_at = clock_timestamp()
WHERE operation_id = sqlc.arg(operation_id)::text
  AND finished_at IS NULL;

-- name: AdvanceDefinitionRunNextStep :execrows
UPDATE eregion_definition_runs
SET next_step = next_step + 1
WHERE operation_id = sqlc.arg(operation_id)::text
  AND next_step = sqlc.arg(expected_next_step)::integer;

-- name: ListUnfinishedTerminalDefinitionRuns :many
SELECT r.operation_id
FROM eregion_definition_runs r
JOIN eregion_operations o ON o.id = r.operation_id
WHERE r.finished_at IS NULL
  AND o.state IN ('succeeded','failed','cancelled')
ORDER BY o.updated_at
LIMIT sqlc.arg(max_rows)::integer;

-- name: DefinitionRunBlocksAdmission :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_definition_runs r
    JOIN eregion_platform_environments e ON e.id = r.environment_id
    JOIN eregion_platform_apps a ON a.id = e.app_id
    WHERE a.name = sqlc.arg(app)::text
      AND r.finished_at IS NULL
      AND (sqlc.arg(environment)::text = '' OR e.name = sqlc.arg(environment)::text)
      AND NOT EXISTS (
        SELECT 1
        FROM eregion_definition_steps st
        JOIN eregion_operations p ON p.id = r.operation_id
        WHERE st.parent_operation_id = r.operation_id
          AND st.ordinal = r.next_step
          AND st.child_operation_id = sqlc.arg(child_operation_id)::text
          AND st.kind = sqlc.arg(kind)::text
          AND st.target = sqlc.arg(target)::text
          AND st.input = sqlc.arg(input)::jsonb
          AND p.state = 'running'
          AND p.kind = sqlc.arg(parent_kind)::text
          AND p.actor_kind = sqlc.arg(actor_kind)::text
          AND p.actor_subject = sqlc.arg(actor_subject)::text));
