-- Persistent environment deletion protection. production and staging are
-- protected by default at creation; every other environment name is
-- unprotected by default. An application deletion always needs the D7 human
-- factor, and deleting the last environment also deletes the application, so a
-- last-environment delete needs the factor even when the environment itself is
-- unprotected. Lowering protection from true to false needs the factor;
-- raising it does not.
--
-- Safe policy for existing rows: environments predate this column, so they are
-- all backfilled protected. An operator can lower protection explicitly with
-- the human factor. This fails closed instead of silently making existing
-- environments agent-deletable.
ALTER TABLE eregion_platform_environments
    ADD COLUMN protected boolean NOT NULL DEFAULT true;

-- Keep the database default true as a fail-closed guard for a row written by
-- an older binary during a rolling upgrade. Application creation always writes
-- the explicit name default: production/staging true, any other name false.
UPDATE eregion_platform_environments SET protected = true;
