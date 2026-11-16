-- Definition DNS journal and recovery reads. The child journal is one joined
-- SQL snapshot; callers lock parent/run/step/child rows separately in the
-- established order before using it.

-- name: LoadDefinitionDNSJournal :one
SELECT r.operation_id, s.ordinal, r.environment_id,
       coalesce(s.domain_id, ''), coalesce(s.allocated_domain_id, ''),
       r.approved, s.kind, s.target, s.input, s.dns_plan, s.dns_cleanup, s.dns_progress,
       p.kind, p.target, p.actor_kind, p.actor_subject, p.state,
       c.kind, c.target, c.actor_kind, c.actor_subject, c.state, c.attempt,
       c.input_hash, c.input, (r.finished_at IS NOT NULL)::boolean AS finished
FROM eregion_definition_steps s
JOIN eregion_definition_runs r ON r.operation_id = s.parent_operation_id
JOIN eregion_operations p ON p.id = r.operation_id
JOIN eregion_operations c ON c.id = s.child_operation_id
WHERE s.parent_operation_id = sqlc.arg(parent_operation_id)::text
  AND s.ordinal = sqlc.arg(ordinal)::integer
  AND s.child_operation_id = sqlc.arg(child_operation_id)::text;

-- name: GetDefinitionDNSChildTarget :one
SELECT a.name, e.name, e.id, s.parent_operation_id, s.ordinal
FROM eregion_definition_steps s
JOIN eregion_definition_runs r ON r.operation_id = s.parent_operation_id
JOIN eregion_platform_environments e ON e.id = r.environment_id
JOIN eregion_platform_apps a ON a.id = e.app_id
WHERE s.child_operation_id = sqlc.arg(child_operation_id)::text;

-- name: DefinitionDNSLinkedCleanupPending :one
SELECT EXISTS(
    SELECT 1 FROM eregion_definition_steps s
    JOIN eregion_definition_runs r ON r.operation_id = s.parent_operation_id
    WHERE s.child_operation_id = sqlc.arg(child_operation_id)::text
      AND r.finished_at IS NULL
      AND (s.dns_plan IS NOT NULL OR s.dns_cleanup IS NOT NULL)
);

-- name: DefinitionDNSTerminalMarker :one
SELECT left(idempotency_key, 11) = 'definition:' AS definition_key,
       (coalesce(input->>'dns_mode', '') = 'cloudflare'
        OR EXISTS(SELECT 1 FROM eregion_platform_domain_dns WHERE last_operation_id = sqlc.arg(operation_id)::text))::boolean AS dns_evidence
FROM eregion_operations
WHERE id = sqlc.arg(operation_id)::text;

-- name: DefinitionDNSCleanupVerifiedWithoutLink :one
SELECT (
    EXISTS(SELECT 1 FROM eregion_operation_events
           WHERE operation_id = sqlc.arg(operation_id)::text
             AND event->>'stage' = 'dns'
             AND event->>'message' LIKE 'Verified Cloudflare cleanup for %')
    AND NOT EXISTS(
        SELECT 1 FROM eregion_definition_steps s
        JOIN eregion_platform_domains d ON d.id = s.domain_id
        WHERE s.child_operation_id = sqlc.arg(operation_id)::text
    )
)::boolean;
