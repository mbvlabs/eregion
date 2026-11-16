-- name: GetAccessGrantRecord :one
SELECT record
FROM eregion_access_grants
WHERE id = sqlc.arg(id)::text;

-- name: ListAccessGrantRecords :many
SELECT record
FROM eregion_access_grants
ORDER BY id;

-- name: InsertAccessGrantIfAbsent :exec
INSERT INTO eregion_access_grants (id, record, created_at)
VALUES (sqlc.arg(id)::text, sqlc.arg(record)::jsonb, clock_timestamp())
ON CONFLICT DO NOTHING;

-- name: LockAccessGrantRecord :one
SELECT record
FROM eregion_access_grants
WHERE id = sqlc.arg(id)::text
FOR UPDATE;

-- name: UpdateAccessGrantRecord :exec
UPDATE eregion_access_grants
SET record = sqlc.arg(record)::jsonb
WHERE id = sqlc.arg(id)::text;
