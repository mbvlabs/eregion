-- Operation event persistence and the retained-history reads the control plane
-- performs against eregion_operation_events alone. Event payloads stay JSON so
-- the contract.Event format remains the single wire representation.

-- name: InsertOperationEvent :exec
INSERT INTO eregion_operation_events (operation_id, sequence, event)
VALUES (sqlc.arg(operation_id)::text, sqlc.arg(sequence)::bigint, sqlc.arg(event)::jsonb);

-- name: ListOperationEvents :many
SELECT sequence, event
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND sequence > sqlc.arg(after_sequence)::bigint
  AND sequence <= sqlc.arg(through_sequence)::bigint
ORDER BY sequence
LIMIT sqlc.arg(max_rows)::bigint;

-- name: GetLatestOperationResult :one
SELECT (event->'result')::jsonb AS result
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND sequence <= sqlc.arg(through_sequence)::bigint
  AND event ? 'result'
ORDER BY sequence DESC
LIMIT 1;

-- name: OperationResultExists :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_operation_events
    WHERE operation_id = sqlc.arg(operation_id)::text
      AND event->'result'->>'attempt' = sqlc.arg(attempt)::text
      AND event->'result'->>'status' = sqlc.arg(status)::text
      AND coalesce(event->'result'->>'final', 'false') = sqlc.arg(final)::text
);

-- name: OperationAttemptStartedAt :one
SELECT min((event->>'at')::timestamptz)::timestamptz AS started_at
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND event->>'attempt' = sqlc.arg(attempt)::text;

-- name: GetFailedStateEventForAttempt :one
SELECT event
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND sequence <= sqlc.arg(through_sequence)::bigint
  AND event->>'kind' = 'state'
  AND event->>'state' = 'failed'
  AND event->>'attempt' = sqlc.arg(attempt)::text
ORDER BY sequence DESC
LIMIT 1;

-- name: OperationActionOutputExists :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_operation_events
    WHERE operation_id = sqlc.arg(operation_id)::text
      AND event ? 'action_output_status'
);

-- name: GetLatestActionOutput :one
SELECT sequence,
       coalesce(event->'action_output', 'null'::jsonb)::jsonb AS action_output,
       (event->>'action_output_status')::text AS action_output_status
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND event ? 'action_output_status'
ORDER BY sequence DESC
LIMIT 1;

-- name: OperationNodeEvidenceExists :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_operation_events
    WHERE operation_id = sqlc.arg(operation_id)::text
      AND event->'node_evidence'->>'execution_id' = sqlc.arg(execution_id)::text
      AND event->'node_evidence'->>'outcome' = sqlc.arg(outcome)::text
);

-- name: GetFailedNodeEvidence :one
SELECT sequence, (event->'node_evidence')::jsonb AS node_evidence
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND event->'node_evidence'->>'attempt' = sqlc.arg(attempt)::text
  AND event->'node_evidence'->>'outcome' = 'failed'
  AND sequence <= sqlc.arg(through_sequence)::bigint
  AND (sqlc.arg(stage)::text = 'action' OR event->>'step' = sqlc.arg(stage)::text)
ORDER BY sequence
LIMIT 1;

-- name: OperationEvidenceLost :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_operation_events lost
    WHERE lost.operation_id = sqlc.arg(operation_id)::text
      AND lost.event->'evidence_loss'->>'attempt' = sqlc.arg(attempt)::text
      AND lost.sequence <= sqlc.arg(through_sequence)::bigint
      AND lost.event->>'stage' = 'evidence'
      AND lost.event->>'message' = 'Node execution evidence could not be retained.'
      AND NOT EXISTS (
          SELECT 1
          FROM eregion_operation_events recovered
          WHERE recovered.operation_id = lost.operation_id
            AND recovered.sequence <= sqlc.arg(through_sequence)::bigint
            AND recovered.event->'node_evidence'->>'execution_id' =
                lost.event->'evidence_loss'->>'execution_id'
      )
);

-- name: OperationEventStageExists :one
SELECT EXISTS (
    SELECT 1
    FROM eregion_operation_events
    WHERE operation_id = sqlc.arg(operation_id)::text
      AND event->>'stage' = sqlc.arg(stage)::text
);

-- name: GetLatestOperationEventMessageByStage :one
SELECT coalesce((
    SELECT event->>'message'
    FROM eregion_operation_events
    WHERE operation_id = sqlc.arg(operation_id)::text
      AND event->>'stage' = sqlc.arg(stage)::text
    ORDER BY sequence DESC
    LIMIT 1
)::text, '')::text AS message;
