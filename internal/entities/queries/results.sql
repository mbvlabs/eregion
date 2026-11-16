-- Result and action projection reads that are not already covered by the
-- operations event primitives. Both are single-statement snapshots.

-- name: ListCandidateDiagnosticEvents :many
SELECT sequence, coalesce(event->'candidate_diagnostic'->>'status', '')::text,
       coalesce(event->>'message', '')::text,
       coalesce(event->'candidate_diagnostic'->>'redaction_context', '')::text,
       coalesce(event->>'step', '')::text,
       coalesce(event->'candidate_diagnostic'->>'deployment_id', '')::text
FROM eregion_operation_events
WHERE operation_id = sqlc.arg(operation_id)::text
  AND event->>'attempt' = sqlc.arg(attempt)::text
  AND event ? 'candidate_diagnostic'
  AND sequence <= sqlc.arg(through_sequence)::bigint
ORDER BY sequence;

-- name: GetFailedResultStage :one
SELECT coalesce(
    (SELECT event->>'stage' FROM eregion_operation_events
     WHERE operation_id = sqlc.arg(operation_id)::text
       AND event->>'attempt' = sqlc.arg(attempt_text)::text
       AND event->>'kind' = 'warning'
       AND event->>'message' LIKE 'Node step failed:%'
       AND coalesce(event->>'stage', '') <> ''
     ORDER BY sequence LIMIT 1),
    (SELECT step FROM eregion_operation_steps
     WHERE operation_id = sqlc.arg(operation_id)::text
       AND attempt = sqlc.arg(attempt)::integer
       AND state = 'failed'
     ORDER BY completed_at NULLS LAST, started_at LIMIT 1),
    'action')::text;
