-- D30 follow-ups: durable cache measurement and registry GC follow-up.
--
-- 017 is published in an edge build, so this is a new migration.
--
-- eregion_retention_runs.cache_bytes is NULL when the node could not size one
-- or more Buildpacks cache volumes. A healthy empty cache is 0, never NULL.
-- Doctor reads the latest non-stale measurement instead of walking Docker
-- volumes and the filesystem synchronously.
--
-- retention_gc_pending survives an operation or worker restart: a registry
-- manifest delete sets it before the delete; a successful GC clears it. Any
-- later retention run drains it when registry protection is resolvable.

ALTER TABLE eregion_retention_runs
    ADD COLUMN cache_bytes bigint
        CHECK (cache_bytes IS NULL OR cache_bytes >= 0),
    ADD COLUMN cache_measured_at timestamptz,
    ADD COLUMN cache_measurement_error text NOT NULL DEFAULT '';

ALTER TABLE eregion_server_settings
    ADD COLUMN retention_gc_pending boolean NOT NULL DEFAULT false;
