-- D30 disk retention: policy, pruned-release state and run observability.
--
-- The server-level default is one server_settings row. An absent row, or an
-- empty retention block, means the D30 defaults defined in internal/retention.
-- Each environment may override the release-scoped fields in its immutable
-- revision, so changing it commits a revision like every other piece of
-- desired state.
--
-- Release rows are never deleted. A pruned release records pruned_at and a
-- deploy or rollback to it refuses with precondition_failed.

CREATE TABLE eregion_server_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    retention jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(retention) = 'object'),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

ALTER TABLE eregion_platform_environment_revisions
    ADD COLUMN retention jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(retention) = 'object');

ALTER TABLE eregion_platform_releases
    ADD COLUMN pruned_at timestamptz;

-- One row per planner run: what it planned, what it removed, how much was
-- reclaimed, and per-item errors. The operation link is optional so a run can
-- be recorded by a maintenance planner that owns no submitted operation.
CREATE TABLE eregion_retention_runs (
    id text PRIMARY KEY CHECK (id ~ '^rtr_[0-9a-f]{32}$'),
    operation_id text REFERENCES eregion_operations(id),
    status text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','running','succeeded','failed','cancelled')),
    planned_items jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(planned_items) = 'array'),
    removed_items jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(removed_items) = 'array'),
    planned_bytes bigint NOT NULL DEFAULT 0 CHECK (planned_bytes >= 0),
    reclaimed_bytes bigint NOT NULL DEFAULT 0 CHECK (reclaimed_bytes >= 0),
    errors jsonb NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(errors) = 'array'),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    CHECK (finished_at IS NULL OR finished_at >= started_at)
);

CREATE INDEX eregion_retention_runs_started
    ON eregion_retention_runs (started_at DESC);
