-- Webhook receipt persistence. The primary key (environment_id,
-- delivery_id_hash) is the replay fence. Job insertion stays on
-- StoreTx.Jobs() in the same transaction; this file has no River insert.

-- name: InsertWebhookReceipt :one
INSERT INTO eregion_webhook_deliveries
    (delivery_id_hash, body_sha256, actor_kind, actor_subject, kind, target, state,
     environment_id, revision_id, secret_revision, secret_revision_hash,
     repository, branch, commit_sha, job_id, admitted_at, admission_attempts,
     first_seen_at, error_code, error_detail)
VALUES (sqlc.arg(delivery_id_hash)::text, sqlc.arg(body_sha256)::text,
        sqlc.arg(actor_kind)::text, sqlc.arg(actor_subject)::text,
        sqlc.arg(kind)::text, sqlc.arg(target)::text, 'admitted',
        sqlc.arg(environment_id)::text, sqlc.arg(revision_id)::text,
        sqlc.arg(secret_revision)::bigint, sqlc.arg(secret_revision_hash)::text,
        sqlc.arg(repository)::text, sqlc.arg(branch)::text, sqlc.arg(commit_sha)::text,
        sqlc.narg(job_id)::bigint, clock_timestamp(), 1, clock_timestamp(), '', '')
ON CONFLICT (environment_id, delivery_id_hash) DO NOTHING
RETURNING first_seen_at;

-- name: GetWebhookReceipt :one
SELECT delivery_id_hash, body_sha256, actor_kind, actor_subject, kind, target, state,
       coalesce(operation_id, ''), coalesce(job_id, 0), admission_attempts,
       error_code, first_seen_at, environment_id, revision_id, secret_revision,
       secret_revision_hash, repository, branch, commit_sha
FROM eregion_webhook_deliveries
WHERE environment_id = sqlc.arg(environment_id)::text
  AND delivery_id_hash = sqlc.arg(delivery_id_hash)::text;

-- name: LockWebhookReceipt :one
SELECT delivery_id_hash, body_sha256, actor_kind, actor_subject, kind, target, state,
       coalesce(operation_id, ''), coalesce(job_id, 0), admission_attempts,
       error_code, first_seen_at, environment_id, revision_id, secret_revision,
       secret_revision_hash, repository, branch, commit_sha
FROM eregion_webhook_deliveries
WHERE environment_id = sqlc.arg(environment_id)::text
  AND delivery_id_hash = sqlc.arg(delivery_id_hash)::text
FOR UPDATE;

-- name: SubmitWebhookReceipt :execrows
UPDATE eregion_webhook_deliveries
SET state = 'submitted', operation_id = sqlc.arg(operation_id)::text,
    submitted_at = clock_timestamp()
WHERE environment_id = sqlc.arg(environment_id)::text
  AND delivery_id_hash = sqlc.arg(delivery_id_hash)::text
  AND state = sqlc.arg(expected_state)::text;

-- name: FailWebhookReceipt :execrows
UPDATE eregion_webhook_deliveries
SET state = 'failed', failed_at = clock_timestamp(),
    error_code = sqlc.arg(error_code)::text, error_detail = sqlc.arg(error_detail)::text
WHERE environment_id = sqlc.arg(environment_id)::text
  AND delivery_id_hash = sqlc.arg(delivery_id_hash)::text
  AND state = 'admitted';

-- name: ReassignWebhookReceiptJob :execrows
UPDATE eregion_webhook_deliveries
SET job_id = sqlc.arg(job_id)::bigint,
    admission_attempts = admission_attempts + 1
WHERE environment_id = sqlc.arg(environment_id)::text
  AND delivery_id_hash = sqlc.arg(delivery_id_hash)::text
  AND state = 'admitted';
