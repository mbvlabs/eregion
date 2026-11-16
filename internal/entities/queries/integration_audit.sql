-- name: LockIntegrationAuditWriters :exec
SELECT pg_advisory_xact_lock(sqlc.arg(lock_key)::bigint);

-- name: LastIntegrationAuditID :one
SELECT COALESCE(MAX(id), 0)::bigint
FROM eregion_integration_audit;

-- name: InsertIntegrationAuditEntry :exec
INSERT INTO eregion_integration_audit (id, entry, recorded_at)
VALUES (sqlc.arg(id)::bigint, sqlc.arg(entry)::jsonb, clock_timestamp());
