-- A retention run may complete with per-item skips: a deterministic node
-- refusal (for example an image Docker still considers in use) is recorded as
-- a skip instead of failing the run and retrying eight times. The run status
-- distinguishes that outcome so doctor can pass with an informational note.

ALTER TABLE eregion_retention_runs
    DROP CONSTRAINT IF EXISTS eregion_retention_runs_status_check;

ALTER TABLE eregion_retention_runs
    ADD CONSTRAINT eregion_retention_runs_status_check
    CHECK (status IN ('pending','running','succeeded','succeeded_with_skips','failed','cancelled'));
