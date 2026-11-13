-- +goose Up

CREATE TABLE eregion_server_settings (
    singleton boolean PRIMARY KEY,
    retention jsonb NOT NULL,
    updated_at timestamptz NOT NULL,
    retention_gc_pending boolean NOT NULL
);

CREATE TABLE eregion_retention_runs (
    id text PRIMARY KEY,
    operation_id text REFERENCES eregion_operations(id),
    status text NOT NULL,
    planned_items jsonb NOT NULL,
    removed_items jsonb NOT NULL,
    planned_bytes bigint NOT NULL,
    reclaimed_bytes bigint NOT NULL,
    errors jsonb NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    cache_bytes bigint,
    cache_measured_at timestamptz,
    cache_measurement_error text NOT NULL,
    gc_generation integer NOT NULL
);

CREATE INDEX eregion_retention_runs_started ON eregion_retention_runs (started_at DESC);
