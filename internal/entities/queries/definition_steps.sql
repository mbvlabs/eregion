-- Definition step journal. These statements preserve the existing SQL and
-- lock order. A child link is one compare-and-set update; journal guards are
-- single-statement reads so a caller never reconstructs a snapshot from
-- several application queries.

-- name: InsertDefinitionStep :exec
INSERT INTO eregion_definition_steps
    (parent_operation_id, ordinal, kind, target, input, domain_id, allocated_domain_id,
     dns_plan, dns_cleanup, dns_progress)
VALUES (sqlc.arg(parent_operation_id)::text, sqlc.arg(ordinal)::integer,
        sqlc.arg(kind)::text, sqlc.arg(target)::text, sqlc.arg(input)::jsonb,
        NULLIF(sqlc.arg(domain_id)::text, ''), NULLIF(sqlc.arg(allocated_domain_id)::text, ''),
        sqlc.narg(dns_plan)::jsonb, sqlc.narg(dns_cleanup)::jsonb, '[]'::jsonb);

-- name: LockDefinitionStep :one
SELECT kind, target, input, child_operation_id
FROM eregion_definition_steps
WHERE parent_operation_id = sqlc.arg(parent_operation_id)::text
  AND ordinal = sqlc.arg(ordinal)::integer
FOR UPDATE;

-- name: SetDefinitionStepChild :execrows
UPDATE eregion_definition_steps
SET child_operation_id = sqlc.arg(child_operation_id)::text
WHERE parent_operation_id = sqlc.arg(parent_operation_id)::text
  AND ordinal = sqlc.arg(ordinal)::integer;

-- name: DefinitionStepChildExists :one
SELECT EXISTS(
    SELECT 1 FROM eregion_definition_steps
    WHERE child_operation_id = sqlc.arg(child_operation_id)::text
);

-- name: ListDefinitionStepChildJournal :many
SELECT s.ordinal, s.kind, s.target, s.input,
       coalesce(c.id, ''), coalesce(c.kind, ''), coalesce(c.target, ''),
       coalesce(c.input, 'null'::jsonb), coalesce(c.state, ''),
       coalesce(c.actor_kind, ''), coalesce(c.actor_subject, ''),
       coalesce(c.idempotency_key, ''), p.actor_kind, p.actor_subject
FROM eregion_definition_steps s
JOIN eregion_operations p ON p.id = s.parent_operation_id
LEFT JOIN eregion_operations c ON c.id = s.child_operation_id
WHERE s.parent_operation_id = sqlc.arg(parent_operation_id)::text
ORDER BY s.ordinal;

-- name: ListActiveDefinitionChildren :many
SELECT c.id
FROM eregion_definition_steps s
JOIN eregion_operations c ON c.id = s.child_operation_id
WHERE s.parent_operation_id = sqlc.arg(parent_operation_id)::text
  AND c.state IN ('queued','running','cancelling')
ORDER BY s.ordinal;

-- name: DefinitionStepCursorBounds :one
SELECT
    EXISTS(SELECT 1 FROM eregion_definition_steps
           WHERE parent_operation_id = sqlc.arg(parent_operation_id)::text
             AND ordinal >= sqlc.arg(next_ordinal)::integer) AS later,
    EXISTS(SELECT 1 FROM eregion_definition_steps s
           JOIN eregion_operations c ON c.id = s.child_operation_id
           WHERE s.parent_operation_id = sqlc.arg(parent_operation_id)::text
             AND c.state IN ('queued','running','cancelling')) AS active;

-- name: DefinitionActiveChildOutsideOrdinal :one
SELECT EXISTS(
    SELECT 1 FROM eregion_definition_steps s
    JOIN eregion_operations c ON c.id = s.child_operation_id
    WHERE s.parent_operation_id = sqlc.arg(parent_operation_id)::text
      AND s.ordinal <> sqlc.arg(ordinal)::integer
      AND c.state IN ('queued','running','cancelling')
);

-- name: GetDefinitionStepLinkedChild :one
SELECT o.state, o.error_code,
       (o.kind = sqlc.arg(kind)::text
        AND o.target = sqlc.arg(target)::text
        AND o.input = sqlc.arg(input)::jsonb
        AND o.actor_kind = sqlc.arg(actor_kind)::text
        AND o.actor_subject = sqlc.arg(actor_subject)::text)::boolean AS matches
FROM eregion_operations o
WHERE o.id = sqlc.arg(child_id)::text;

-- name: DefinitionChildJournalReady :one
SELECT EXISTS(
    SELECT 1
    FROM eregion_definition_runs r
    JOIN eregion_definition_steps s ON s.parent_operation_id = r.operation_id
    WHERE r.operation_id = sqlc.arg(operation_id)::text
      AND r.finished_at IS NULL
      AND r.next_step = sqlc.arg(next_step)::integer
      AND r.environment_id = sqlc.arg(environment_id)::text
      AND s.ordinal = sqlc.arg(next_step)::integer
      AND s.kind = sqlc.arg(kind)::text
      AND s.target = sqlc.arg(target)::text
      AND s.input = sqlc.arg(input)::jsonb
      AND s.child_operation_id IS NULL
);

-- name: SaveDefinitionDNSProgress :execrows
UPDATE eregion_definition_steps
SET dns_progress = sqlc.arg(dns_progress)::jsonb
WHERE parent_operation_id = sqlc.arg(parent_operation_id)::text
  AND ordinal = sqlc.arg(ordinal)::integer
  AND dns_progress = sqlc.arg(expected_progress)::jsonb
  AND child_operation_id = sqlc.arg(child_operation_id)::text;
