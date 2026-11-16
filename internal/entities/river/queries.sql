-- App-owned River state/rescue/reconcile statements. The predicates are the
-- existing control-plane predicates: queue + running state, attempt and the
-- observed last attempted_by client for rescue, and the exact state sets for
-- reconcile. River's own library SQL is not copied. There is no River INSERT or
-- DELETE in this file; enqueue uses the existing typed StoreTx.Jobs() path.

-- name: ListRunningRiverJobsForQueue :many
SELECT j.id, j.attempted_by, coalesce(j.args->>'operation_id', '')::text,
       j.attempt, j.max_attempts
FROM river_job j
WHERE j.queue = sqlc.arg(queue)::text AND j.state = 'running';

-- name: GetRiverJobForUpdate :one
SELECT state FROM river_job WHERE id = sqlc.arg(id)::bigint FOR UPDATE;

-- name: GetRiverJobState :one
SELECT state FROM river_job WHERE id = sqlc.arg(id)::bigint;

-- name: DiscardRunningRiverJob :execrows
UPDATE river_job
SET state = 'discarded', finalized_at = clock_timestamp(),
    errors = array_append(coalesce(errors, ARRAY[]::jsonb[]),
        jsonb_build_object('at', clock_timestamp(), 'attempt', attempt,
                           'error', sqlc.arg(reason)::text, 'trace', ''))
WHERE id = sqlc.arg(id)::bigint
  AND queue = sqlc.arg(queue)::text
  AND state = 'running'
  AND attempt = sqlc.arg(attempt)::bigint
  AND cardinality(attempted_by) > 0
  AND attempted_by[cardinality(attempted_by)] = sqlc.arg(last_client)::text;

-- name: RequeueRunningRiverJob :execrows
UPDATE river_job
SET state = 'available', scheduled_at = clock_timestamp(), finalized_at = NULL
WHERE id = sqlc.arg(id)::bigint
  AND queue = sqlc.arg(queue)::text
  AND state = 'running'
  AND attempt = sqlc.arg(attempt)::bigint
  AND cardinality(attempted_by) > 0
  AND attempted_by[cardinality(attempted_by)] = sqlc.arg(last_client)::text;

-- name: SnoozeRiverJobWithAttempt :execrows
UPDATE river_job
SET state = 'scheduled', finalized_at = NULL,
    attempt = GREATEST(attempt, sqlc.arg(attempt)::bigint),
    max_attempts = GREATEST(max_attempts, attempt + 1, sqlc.arg(attempt)::bigint + 1),
    scheduled_at = clock_timestamp() + (sqlc.arg(delay_seconds)::integer * interval '1 second')
WHERE id = sqlc.arg(id)::bigint;

-- name: RescheduleRiverJob :execrows
UPDATE river_job
SET state = 'scheduled', finalized_at = NULL,
    max_attempts = GREATEST(max_attempts, attempt + 1),
    scheduled_at = clock_timestamp() + (sqlc.arg(delay_seconds)::integer * interval '1 second')
WHERE id = sqlc.arg(id)::bigint;

-- name: ListReconcileCandidateOperations :many
SELECT o.id
FROM eregion_operations o
LEFT JOIN river_job j ON j.id = o.job_id
WHERE o.state IN ('queued','running','cancelling')
  AND (j.id IS NULL OR j.state IN ('discarded','cancelled','completed'))
ORDER BY o.id
LIMIT sqlc.arg(max_rows)::integer;

-- name: ListAdmittedWebhookReceiptsNeedingRiver :many
SELECT d.environment_id, d.delivery_id_hash
FROM eregion_webhook_deliveries d
LEFT JOIN river_job j ON j.id = d.job_id
WHERE d.state = 'admitted'
  AND (d.job_id IS NULL OR j.id IS NULL OR j.state IN ('completed','discarded','cancelled'))
ORDER BY d.first_seen_at
LIMIT sqlc.arg(max_rows)::integer;
