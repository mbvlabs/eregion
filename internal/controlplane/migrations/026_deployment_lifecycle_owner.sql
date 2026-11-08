-- D17: one durable deployment lifecycle owner per environment.
--
-- The owner row is the only source of route intent. Route writers must not
-- elect a candidate by deployment creation time or by a running step: a running
-- route step means intent, not proof that the apply happened. The owner freezes
-- the exact predecessor and restore target when it is acquired under the
-- environment advisory lock, and it is retained across worker retries,
-- cancellation and restoration until the terminal traffic state is safe.
--
-- Legacy rows are upgraded conservatively. If an environment has exactly one
-- active deployment, it becomes the owner with the recorded serving route as
-- its initial intent. If several are active, the row is ambiguous, blocks new
-- work and points route writers at the recorded serving deployment until a
-- maintenance reconciliation settles it without electing by timestamp.

CREATE TABLE eregion_platform_deployment_owners (
    environment_id text PRIMARY KEY REFERENCES eregion_platform_environments(id) ON DELETE CASCADE,
    operation_id text REFERENCES eregion_operations(id) ON DELETE CASCADE,
    deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE CASCADE,
    owner_attempt integer CHECK (owner_attempt IS NULL OR owner_attempt >= 0),
    phase text NOT NULL CHECK (phase IN ('prepared','switching','verifying','serving','restoring','restored','ambiguous')),
    route_target_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    restore_release_id text REFERENCES eregion_platform_releases(id),
    restore_deployment_id text REFERENCES eregion_platform_deployments(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (
        (phase = 'ambiguous' AND operation_id IS NULL AND deployment_id IS NULL AND owner_attempt IS NULL)
        OR
        (phase <> 'ambiguous' AND operation_id IS NOT NULL AND deployment_id IS NOT NULL AND owner_attempt IS NOT NULL)
    )
);

CREATE INDEX eregion_platform_deployment_owners_deployment
    ON eregion_platform_deployment_owners (deployment_id);

-- Legacy rows are not backfilled. Inferring an owner for an in-flight
-- deployment would silently elect route intent, and a pending/running
-- deployment may own containers the owner table cannot reconstruct safely.
-- The cutover therefore fails closed: finish or cancel deployment lifecycle
-- work before upgrading. A safe database gets the empty owner table and
-- existing terminal deployments remain history; route writers continue to use
-- the recorded serving deployment until a new operation acquires an owner.

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM eregion_operations
        WHERE state IN ('queued','running','cancelling')
          AND kind IN ('platform.deploy','platform.rollback','platform.restart','platform.promote')
    ) THEN
        RAISE EXCEPTION 'migration 026: in-flight deployment lifecycle operation; finish or cancel it before upgrade';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM eregion_platform_deployments
        WHERE state IN ('pending','running')
    ) THEN
        RAISE EXCEPTION 'migration 026: pending or running deployment row; finish or remove it before upgrade';
    END IF;
END $$;
