-- Retention runs and server settings. These statements preserve the existing
-- plan/retry semantics: the newest run for an operation is reused, JSON items
-- stay exact, and the GC pending flag is node-wide, not owned by one run.

-- name: GetServerRetentionSettings :one
SELECT retention, retention_gc_pending, updated_at
FROM eregion_server_settings
WHERE singleton;

-- name: SetServerRetentionPolicy :exec
INSERT INTO eregion_server_settings
    (singleton, retention, retention_gc_pending, updated_at)
VALUES (true, sqlc.arg(retention)::jsonb, false, clock_timestamp())
ON CONFLICT (singleton) DO UPDATE
SET retention = EXCLUDED.retention, updated_at = clock_timestamp();

-- name: GetRetentionGCPending :one
SELECT coalesce(
    (SELECT retention_gc_pending FROM eregion_server_settings WHERE singleton), false)::boolean;

-- name: SetRetentionGCPending :exec
INSERT INTO eregion_server_settings
    (singleton, retention_gc_pending, retention, updated_at)
VALUES (true, sqlc.arg(pending)::boolean, '{}'::jsonb, clock_timestamp())
ON CONFLICT (singleton) DO UPDATE
SET retention_gc_pending = EXCLUDED.retention_gc_pending,
    updated_at = clock_timestamp();

-- name: GetLatestRetentionRunForOperation :one
SELECT id, coalesce(operation_id, ''), status, planned_items, removed_items,
       planned_bytes, reclaimed_bytes, errors, started_at, finished_at,
       cache_bytes, cache_measured_at, cache_measurement_error, gc_generation
FROM eregion_retention_runs
WHERE operation_id = sqlc.arg(operation_id)::text
ORDER BY started_at DESC, id DESC
LIMIT 1;

-- name: InsertRetentionRun :exec
INSERT INTO eregion_retention_runs
    (id, operation_id, status, planned_items, removed_items, planned_bytes,
     reclaimed_bytes, errors, started_at, finished_at, cache_bytes,
     cache_measured_at, cache_measurement_error, gc_generation)
VALUES (sqlc.arg(id)::text, sqlc.narg(operation_id)::text, sqlc.arg(status)::text,
        sqlc.arg(planned_items)::jsonb, sqlc.arg(removed_items)::jsonb,
        sqlc.arg(planned_bytes)::bigint, sqlc.arg(reclaimed_bytes)::bigint,
        sqlc.arg(errors)::jsonb, sqlc.arg(started_at)::timestamptz,
        sqlc.narg(finished_at)::timestamptz, sqlc.narg(cache_bytes)::bigint,
        sqlc.narg(cache_measured_at)::timestamptz,
        sqlc.arg(cache_measurement_error)::text, sqlc.arg(gc_generation)::integer);

-- name: UpdateRetentionRun :exec
UPDATE eregion_retention_runs
SET status = sqlc.arg(status)::text,
    planned_items = sqlc.arg(planned_items)::jsonb,
    removed_items = sqlc.arg(removed_items)::jsonb,
    planned_bytes = sqlc.arg(planned_bytes)::bigint,
    reclaimed_bytes = sqlc.arg(reclaimed_bytes)::bigint,
    errors = sqlc.arg(errors)::jsonb,
    finished_at = sqlc.narg(finished_at)::timestamptz,
    cache_bytes = sqlc.narg(cache_bytes)::bigint,
    cache_measured_at = sqlc.narg(cache_measured_at)::timestamptz,
    cache_measurement_error = sqlc.arg(cache_measurement_error)::text,
    gc_generation = sqlc.arg(gc_generation)::integer
WHERE id = sqlc.arg(id)::text;

-- name: GetLastRetentionRunFacts :one
SELECT status, started_at, finished_at,
       jsonb_array_length(planned_items), jsonb_array_length(removed_items),
       planned_bytes, reclaimed_bytes, jsonb_array_length(errors), errors,
       coalesce(errors->0->>'message', '')::text,
       cache_bytes, cache_measured_at, cache_measurement_error
FROM eregion_retention_runs
ORDER BY started_at DESC, id DESC
LIMIT 1;

-- name: GetLastSuccessfulRetentionAt :one
SELECT coalesce(finished_at, started_at)
FROM eregion_retention_runs
WHERE status IN ('succeeded','succeeded_with_skips')
ORDER BY finished_at DESC NULLS LAST, started_at DESC
LIMIT 1;
