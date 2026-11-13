-- +goose Up

CREATE TABLE eregion_platform_environment_revisions (
    id text PRIMARY KEY,
    environment_id text NOT NULL REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    number integer NOT NULL,
    operation_id text REFERENCES eregion_operations(id),
    actor_kind text NOT NULL,
    actor_subject text NOT NULL,
    operation text NOT NULL,
    summary text NOT NULL,
    source jsonb NOT NULL,
    auto_deploy boolean NOT NULL,
    webhook_secret_name text NOT NULL,
    runtime jsonb NOT NULL,
    processes jsonb NOT NULL,
    config jsonb NOT NULL,
    secret_names text[] NOT NULL,
    deploy_intent boolean NOT NULL,
    created_at timestamptz NOT NULL,
    retention jsonb NOT NULL,
    resources text[] NOT NULL,
    UNIQUE (environment_id, number)
);

CREATE INDEX eregion_platform_environment_revisions_environment ON eregion_platform_environment_revisions (environment_id, number DESC);
