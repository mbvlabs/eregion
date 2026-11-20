-- name: GetBuildCapacityClaim :one
SELECT node_name, builder_identity, attempt FROM eregion_build_capacity_claims
WHERE operation_id = sqlc.arg(operation_id)::text;

-- name: LockBuildCapacityClaimForStop :one
SELECT claim_id, node_name, builder_identity FROM eregion_build_capacity_claims
WHERE operation_id = sqlc.arg(operation_id)::text FOR UPDATE;

-- name: ListBuildCapacityClaims :many
SELECT c.claim_id, c.operation_id, c.node_name, c.builder_identity, c.attempt, c.created_at, o.state
FROM eregion_build_capacity_claims c
JOIN eregion_operations o ON o.id = c.operation_id
ORDER BY c.created_at, c.claim_id;

-- name: FindBuildCapacityClaim :one
SELECT c.claim_id, c.operation_id, c.node_name, c.builder_identity, c.attempt, c.created_at, o.state
FROM eregion_build_capacity_claims c
JOIN eregion_operations o ON o.id = c.operation_id
WHERE c.operation_id = sqlc.arg(operation_id)::text;

-- name: ListTerminalBuildCapacityClaims :many
SELECT c.claim_id, c.operation_id, c.node_name, c.builder_identity, c.attempt
FROM eregion_build_capacity_claims c
JOIN eregion_operations o ON o.id = c.operation_id
WHERE o.state IN ('succeeded', 'failed', 'cancelled')
ORDER BY c.checked_at NULLS FIRST, c.created_at, c.claim_id
LIMIT sqlc.arg(max_rows)::integer;

-- name: TouchTerminalBuildCapacityClaim :execrows
UPDATE eregion_build_capacity_claims c SET checked_at = clock_timestamp()
WHERE c.claim_id = sqlc.arg(claim_id)::text
  AND c.operation_id = sqlc.arg(operation_id)::text AND c.node_name = sqlc.arg(node_name)::text
  AND c.builder_identity = sqlc.arg(builder_identity)::text
  AND c.attempt = sqlc.arg(attempt)::integer
  AND EXISTS (SELECT 1 FROM eregion_operations o WHERE o.id = c.operation_id
              AND o.state IN ('succeeded', 'failed', 'cancelled'));

-- name: DeleteTerminalBuildCapacityClaim :execrows
DELETE FROM eregion_build_capacity_claims c
WHERE c.claim_id = sqlc.arg(claim_id)::text
  AND c.operation_id = sqlc.arg(operation_id)::text AND c.node_name = sqlc.arg(node_name)::text
  AND c.builder_identity = sqlc.arg(builder_identity)::text
  AND c.attempt = sqlc.arg(attempt)::integer
  AND EXISTS (SELECT 1 FROM eregion_operations o WHERE o.id = c.operation_id
              AND o.state IN ('succeeded', 'failed', 'cancelled'));

-- name: CountBuildCapacityClaims :one
SELECT count(*) FROM eregion_build_capacity_claims
WHERE node_name = sqlc.arg(node_name)::text;

-- name: InsertBuildCapacityClaim :exec
INSERT INTO eregion_build_capacity_claims(operation_id, node_name, builder_identity, attempt)
VALUES (sqlc.arg(operation_id)::text, sqlc.arg(node_name)::text, sqlc.arg(builder_identity)::text, sqlc.arg(attempt)::integer);

-- name: AdvanceBuildCapacityClaim :execrows
UPDATE eregion_build_capacity_claims SET attempt = sqlc.arg(attempt)::integer
WHERE operation_id = sqlc.arg(operation_id)::text AND node_name = sqlc.arg(node_name)::text
  AND attempt <= sqlc.arg(attempt)::integer;

-- name: DeleteBuildCapacityClaim :execrows
DELETE FROM eregion_build_capacity_claims
WHERE operation_id = sqlc.arg(operation_id)::text AND node_name = sqlc.arg(node_name)::text
  AND attempt = sqlc.arg(attempt)::integer;
