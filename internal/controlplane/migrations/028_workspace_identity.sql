-- Portable installation workspace identity.
--
-- This is one singleton non-secret identity for the installation, not a tenant
-- boundary, ACL scope or node registry. The forward migration deliberately
-- inserts nothing: an existing alpha.6 installation stays explicitly
-- unconfigured until an operator runs `apply --workspace NAME`. Reads must not
-- guess a name from a local context or create a row.

CREATE TABLE eregion_workspace (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    workspace_id text NOT NULL UNIQUE CHECK (workspace_id ~ '^ws_[0-9a-f]{24}$'),
    name text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The generated ID and slug name are assigned once. Replay reads the existing
-- row; conflicting assignment is refused by application code and the primary
-- key. The trigger makes an accidental UPDATE/DELETE (or SQL repair) fail rather
-- than silently move the installation identity.
CREATE FUNCTION eregion_workspace_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'eregion workspace identity is immutable';
END;
$$;

CREATE TRIGGER eregion_workspace_immutable
    BEFORE UPDATE OR DELETE ON eregion_workspace
    FOR EACH ROW EXECUTE FUNCTION eregion_workspace_immutable();
