-- A registry garbage-collection pass needs its own durable node-effect
-- identity. The fixed step ID could satisfy a retry from an earlier completed
-- pass and skip blobs orphaned by a later manifest delete. Each new pass
-- increments this per-run generation and suffixes the journaled step ID.

ALTER TABLE eregion_retention_runs
    ADD COLUMN gc_generation integer NOT NULL DEFAULT 0
        CHECK (gc_generation >= 0);
