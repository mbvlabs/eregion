-- Durable webhook delivery deduplication, independent of operation retention.
--
-- GitHub signs no timestamp, so a correctly signed delivery can be replayed.
-- Operation idempotency rows are not a durable replay guard: retention or
-- operator cleanup may remove them while the webhook secret is still valid.
--
-- This table stores only hashes and durable identity:
--   * delivery_id_hash is SHA-256 of X-GitHub-Delivery, never the raw ID;
--   * body_sha256 binds one delivery ID to one exact payload;
--   * operation_id records the accepted operation even after that operation
--     row is purged, so a replay cannot create a new operation.
-- It never stores the webhook secret, raw signature, request body, or token.

CREATE TABLE eregion_webhook_deliveries (
    delivery_id_hash text PRIMARY KEY CHECK (delivery_id_hash ~ '^[0-9a-f]{64}$'),
    body_sha256 text NOT NULL CHECK (body_sha256 ~ '^[0-9a-f]{64}$'),
    actor_kind text NOT NULL CHECK (actor_kind IN ('webhook')),
    actor_subject text NOT NULL CHECK (length(actor_subject) BETWEEN 1 AND 256),
    kind text NOT NULL,
    target text NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','submitted')),
    operation_id text,
    first_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    submitted_at timestamptz,
    CHECK (
        (state = 'pending' AND operation_id IS NULL AND submitted_at IS NULL)
        OR (state = 'submitted' AND operation_id IS NOT NULL AND submitted_at IS NOT NULL)
    )
);

CREATE INDEX eregion_webhook_deliveries_first_seen
    ON eregion_webhook_deliveries (first_seen_at);
