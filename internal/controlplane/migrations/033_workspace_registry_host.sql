-- Immutable local registry-host binding for the singleton workspace.
--
-- The binding is installation metadata, not a fleet/node registry. It records
-- the physical main host that owns the private registry and host lock:
-- node name, public SSH host-key type/fingerprint, and canonical node state
-- directory. Existing workspace rows keep all four columns NULL and remain
-- explicitly unverified; there is no backfill or inferred host identity.
ALTER TABLE eregion_workspace
    ADD COLUMN registry_host_node text,
    ADD COLUMN registry_host_key_type text,
    ADD COLUMN registry_host_key_fingerprint text,
    ADD COLUMN registry_host_state_dir text;

ALTER TABLE eregion_workspace
    ADD CONSTRAINT eregion_workspace_registry_host_all_or_none
    CHECK (num_nonnulls(registry_host_node, registry_host_key_type,
                        registry_host_key_fingerprint, registry_host_state_dir) IN (0, 4));

ALTER TABLE eregion_workspace
    ADD CONSTRAINT eregion_workspace_registry_host_values
    CHECK (
        registry_host_node IS NULL OR (
            length(registry_host_node) BETWEEN 1 AND 64
            AND length(registry_host_key_type) BETWEEN 1 AND 128
            AND length(registry_host_key_fingerprint) BETWEEN 1 AND 256
            AND length(registry_host_state_dir) BETWEEN 1 AND 4096
        )
    );
