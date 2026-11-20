-- +goose Up
CREATE TABLE eregion_build_placements (
    operation_id text PRIMARY KEY REFERENCES eregion_operations(id),
    node_name text NOT NULL,
    node_identity text NOT NULL
);
