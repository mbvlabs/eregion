-- M2 node enrollment. A node row now carries the verified facts of an enrolled
-- node: the pinned SSH host key, the node-generated identity key, the probed
-- host facts, role configuration, and the public side of its WireGuard setup.
-- No private key and no credential is stored here: the node's identity and
-- WireGuard private keys never leave the node, and the control plane's own
-- per-node WireGuard key lives in a protected file on the control host.
ALTER TABLE eregion_platform_nodes
    ADD COLUMN enrollment_state text NOT NULL DEFAULT 'registered'
        CHECK (enrollment_state IN ('registered','pending','enrolled','failed')),
    ADD COLUMN enrollment_error text NOT NULL DEFAULT '',
    ADD COLUMN enroll_operation_id text,
    ADD COLUMN enrolled_at timestamptz,
    ADD COLUMN transport text NOT NULL DEFAULT 'local' CHECK (transport IN ('local','ssh')),
    ADD COLUMN ssh_address text NOT NULL DEFAULT '',
    ADD COLUMN ssh_user text NOT NULL DEFAULT '',
    ADD COLUMN ssh_host_key text NOT NULL DEFAULT '',
    ADD COLUMN ssh_host_key_fingerprint text NOT NULL DEFAULT '',
    ADD COLUMN expected_identity_fingerprint text NOT NULL DEFAULT '',
    ADD COLUMN identity_public_key text NOT NULL DEFAULT '',
    ADD COLUMN identity_fingerprint text NOT NULL DEFAULT '',
    ADD COLUMN os_id text NOT NULL DEFAULT '',
    ADD COLUMN os_version text NOT NULL DEFAULT '',
    ADD COLUMN arch text NOT NULL DEFAULT '',
    ADD COLUMN probe jsonb,
    ADD COLUMN build_concurrency integer NOT NULL DEFAULT 1 CHECK (build_concurrency BETWEEN 1 AND 16),
    ADD COLUMN build_user text NOT NULL DEFAULT '',
    ADD COLUMN wg_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN wg_address text NOT NULL DEFAULT '',
    ADD COLUMN wg_endpoint text NOT NULL DEFAULT '',
    ADD COLUMN wg_ssh_port integer NOT NULL DEFAULT 0 CHECK (wg_ssh_port BETWEEN 0 AND 65535),
    ADD COLUMN wg_public_key text NOT NULL DEFAULT '',
    ADD COLUMN control_wg_public_key text NOT NULL DEFAULT '',
    ADD COLUMN wg_state text NOT NULL DEFAULT 'none' CHECK (wg_state IN ('none','keyed','rendered','applied','failed')),
    ADD COLUMN wg_reconciled_at timestamptz,
    ADD COLUMN last_check_at timestamptz,
    ADD COLUMN last_check_state text NOT NULL DEFAULT 'unknown',
    ADD COLUMN last_check_detail text NOT NULL DEFAULT '';

-- A tunnel address identifies exactly one node.
CREATE UNIQUE INDEX eregion_platform_nodes_wg_address ON eregion_platform_nodes(wg_address) WHERE wg_address <> '';
-- A node-generated identity key identifies exactly one node.
CREATE UNIQUE INDEX eregion_platform_nodes_identity_key ON eregion_platform_nodes(identity_public_key) WHERE identity_public_key <> '';
