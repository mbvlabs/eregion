-- GENERATION-ONLY metadata DDL for the pinned River version.
--
-- Source: github.com/riverqueue/river/riverdriver/riverpgxv5 v0.40.0,
-- migration line "main", versions 1-7. This file is the final river_job
-- metadata shape after those migrations. It is NOT an application migration,
-- is never embedded in internal/dbschema, and is never applied to a database
-- that owns the application migration path. It exists only so sqlc can type
-- the app-owned rescue/reconcile/state statements, and so the real-PG schema
-- audit can compare this generation input with River's own migrated schema.
--
-- River's library queries are an explicit infrastructure exception and are not
-- copied into the application query set.

CREATE TYPE river_job_state AS ENUM (
    'available',
    'cancelled',
    'completed',
    'discarded',
    'pending',
    'retryable',
    'running',
    'scheduled'
);

CREATE TABLE river_job (
    id bigserial PRIMARY KEY,
    state river_job_state NOT NULL DEFAULT 'available',
    attempt smallint NOT NULL DEFAULT 0,
    max_attempts smallint NOT NULL DEFAULT 25,
    attempted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    finalized_at timestamptz,
    scheduled_at timestamptz NOT NULL DEFAULT now(),
    priority smallint NOT NULL DEFAULT 1,
    args jsonb NOT NULL,
    attempted_by text[],
    errors jsonb[],
    kind text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    queue text NOT NULL DEFAULT 'default',
    tags varchar(255)[] NOT NULL DEFAULT '{}',
    unique_key bytea,
    unique_states bit(8),
    CONSTRAINT finalized_or_finalized_at_null CHECK (
        (finalized_at IS NULL AND state NOT IN ('cancelled', 'completed', 'discarded'))
        OR (finalized_at IS NOT NULL AND state IN ('cancelled', 'completed', 'discarded'))
    ),
    CONSTRAINT max_attempts_is_positive CHECK (max_attempts > 0),
    CONSTRAINT priority_in_range CHECK (priority >= 1 AND priority <= 4),
    CONSTRAINT queue_length CHECK (char_length(queue) > 0 AND char_length(queue) < 128),
    CONSTRAINT kind_length CHECK (char_length(kind) > 0 AND char_length(kind) < 128)
);

CREATE INDEX river_job_kind ON river_job USING btree (kind);
CREATE INDEX river_job_state_and_finalized_at_index
    ON river_job USING btree (state, finalized_at)
    WHERE finalized_at IS NOT NULL;
CREATE INDEX river_job_prioritized_fetching_index
    ON river_job USING btree (state, queue, priority, scheduled_at, id);
CREATE INDEX river_job_args_index ON river_job USING GIN (args);
CREATE INDEX river_job_metadata_index ON river_job USING GIN (metadata);

CREATE FUNCTION river_job_state_in_bitmask(bitmask BIT(8), state river_job_state)
RETURNS boolean
LANGUAGE SQL
IMMUTABLE
AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;

CREATE UNIQUE INDEX river_job_unique_idx ON river_job (unique_key)
    WHERE unique_key IS NOT NULL
      AND unique_states IS NOT NULL
      AND river_job_state_in_bitmask(unique_states, state);
