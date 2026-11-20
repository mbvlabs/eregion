-- name: GetBuildPlacement :one
SELECT node_name, node_identity FROM eregion_build_placements
WHERE operation_id = $1;

-- name: InsertBuildPlacement :exec
INSERT INTO eregion_build_placements (operation_id, node_name, node_identity)
VALUES ($1, $2, $3);
