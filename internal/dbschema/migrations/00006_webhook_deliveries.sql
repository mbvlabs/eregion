-- +goose Up

CREATE TABLE eregion_webhook_deliveries (
    delivery_id_hash text NOT NULL,
    body_sha256 text NOT NULL,
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    kind text NOT NULL,
    target text NOT NULL,
    state text NOT NULL,
    operation_id text,
    first_seen_at timestamptz NOT NULL,
    submitted_at timestamptz,
    environment_id text NOT NULL,
    revision_id text NOT NULL,
    secret_revision bigint NOT NULL,
    secret_revision_hash text NOT NULL,
    repository text NOT NULL,
    branch text NOT NULL,
    commit_sha text NOT NULL,
    job_id bigint,
    admitted_at timestamptz,
    failed_at timestamptz,
    error_code text NOT NULL,
    error_detail text NOT NULL,
    admission_attempts integer NOT NULL,
    PRIMARY KEY (environment_id, delivery_id_hash)
);

CREATE INDEX eregion_webhook_deliveries_first_seen ON eregion_webhook_deliveries (first_seen_at);

CREATE INDEX eregion_webhook_deliveries_admitted ON eregion_webhook_deliveries (job_id) WHERE state = 'admitted';
