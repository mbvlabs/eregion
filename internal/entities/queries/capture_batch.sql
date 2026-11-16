-- name: LockCandidateCaptureOperation :one
SELECT attempt, last_sequence
FROM eregion_operations
WHERE id = sqlc.arg(operation_id)
FOR UPDATE;

-- name: LockCandidateCaptureOwner :one
SELECT true AS owned
FROM eregion_platform_deployment_owners
WHERE environment_id = sqlc.arg(environment_id)::text
  AND operation_id = sqlc.arg(operation_id)::text
  AND deployment_id = sqlc.arg(deployment_id)::text
  AND owner_attempt = sqlc.arg(owner_attempt)::integer
FOR UPDATE;

-- name: CandidateCaptureClaimExists :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_operation_events
    WHERE operation_id = sqlc.arg(operation_id)::text
      AND event->>'stage' = sqlc.arg(stage)::text
      AND event->>'attempt' = sqlc.arg(attempt)::text
      AND event->>'step' = sqlc.arg(step)::text
) AS claim_exists;

-- name: AppendCandidateCaptureClaim :execrows
WITH advanced AS (
	UPDATE eregion_operations
	SET last_sequence = sqlc.arg(base_sequence)::bigint + 1, updated_at = sqlc.arg(updated_at)
	WHERE id = sqlc.arg(operation_id) AND last_sequence = sqlc.arg(base_sequence)::bigint
	RETURNING id, last_sequence
)
INSERT INTO eregion_operation_events(operation_id,sequence,event)
SELECT advanced.id, advanced.last_sequence, sqlc.arg(event_json)::jsonb
FROM advanced;

-- name: AppendCaptureEvents :execrows
WITH advanced AS (
	UPDATE eregion_operations
	SET last_sequence = $2::bigint + $3::bigint, updated_at = $4
	WHERE id = $1 AND last_sequence = $2::bigint
	RETURNING id
)
INSERT INTO eregion_operation_events(operation_id,sequence,event)
SELECT advanced.id, batch.sequence, batch.event
FROM advanced
CROSS JOIN jsonb_to_recordset($5::jsonb) AS batch(sequence bigint, event jsonb);
