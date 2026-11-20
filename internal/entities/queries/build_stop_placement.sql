-- name: GetBuildStopPlacement :one
SELECT node_name, node_identity FROM eregion_build_stop_placements
WHERE operation_id = $1;

-- name: InsertBuildStopPlacement :execrows
INSERT INTO eregion_build_stop_placements (operation_id, node_name, node_identity)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;
