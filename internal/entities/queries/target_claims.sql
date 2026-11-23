-- name: InsertTargetClaim :execrows
INSERT INTO eregion_target_claims (token, operation_id)
VALUES (sqlc.arg(token)::text, sqlc.arg(operation_id)::text)
ON CONFLICT DO NOTHING;

-- name: InsertTargetClaimTarget :execrows
INSERT INTO eregion_target_claim_targets (target, token)
VALUES (sqlc.arg(target)::text, sqlc.arg(token)::text)
ON CONFLICT DO NOTHING;

-- name: LockActiveTargetClaim :one
SELECT token FROM eregion_target_claims
WHERE token = sqlc.arg(token)::text AND operation_id = sqlc.arg(operation_id)::text
  AND sealed_at IS NULL AND released_at IS NULL
FOR UPDATE;

-- name: SealTargetClaim :execrows
UPDATE eregion_target_claims SET sealed_at = clock_timestamp()
WHERE token = sqlc.arg(token)::text AND operation_id = sqlc.arg(operation_id)::text
  AND sealed_at IS NULL AND released_at IS NULL;

-- name: ReleaseTargetClaim :execrows
UPDATE eregion_target_claims SET released_at = clock_timestamp()
WHERE token = sqlc.arg(token)::text AND operation_id = sqlc.arg(operation_id)::text
  AND sealed_at IS NOT NULL AND released_at IS NULL;

-- name: DeleteTargetClaimTargets :exec
DELETE FROM eregion_target_claim_targets WHERE token = sqlc.arg(token)::text;

-- name: GetTargetClaim :one
SELECT operation_id, sealed_at, released_at FROM eregion_target_claims
WHERE token = sqlc.arg(token)::text;

-- name: GetUnreleasedTargetClaimByOperation :one
SELECT token, sealed_at FROM eregion_target_claims
WHERE operation_id = sqlc.arg(operation_id)::text AND released_at IS NULL;

-- name: ListTargetClaimTargets :many
SELECT target FROM eregion_target_claim_targets
WHERE token = sqlc.arg(token)::text ORDER BY target;

-- name: ListSettledTargetClaims :many
SELECT c.operation_id, c.token, o.target, o.kind
FROM eregion_target_claims c
JOIN eregion_operations o ON o.id = c.operation_id
WHERE c.released_at IS NULL
  AND (o.state = 'succeeded' OR (
    o.kind IN ('platform.build.retry', 'platform.build.stop')
    AND o.state IN ('failed', 'cancelled')
  ))
  AND NOT EXISTS (
    SELECT 1 FROM eregion_operation_steps s
    WHERE s.operation_id = o.id AND s.state <> 'completed'
      AND NOT EXISTS (
        SELECT 1 FROM eregion_retired_build_effects r
        WHERE o.kind IN ('platform.build.retry', 'platform.build.stop')
          AND r.operation_id = s.operation_id AND r.step = s.step
          AND r.execution_id = s.execution_id AND r.inputs_hash = s.inputs_hash
      )
  )
ORDER BY c.created_at, c.token
LIMIT sqlc.arg(max_rows)::integer;
