-- Environment revisions carry the managed-resource dependency names that gate
-- deployment. Names only: resource readiness is observed separately, and no
-- secret value enters a revision. Deleting an environment still cascades its
-- revisions; a resource container and its named volumes are managed separately
-- and are never removed by this cascade.

ALTER TABLE eregion_platform_environment_revisions
    ADD COLUMN resources text[] NOT NULL DEFAULT '{}';
