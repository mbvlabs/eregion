-- A build may run on a builder node distinct from the application runtime node.
-- An empty value keeps the historical behaviour: builds run on node_name.
ALTER TABLE eregion_platform_environments
    ADD COLUMN build_node text REFERENCES eregion_platform_nodes(name);
