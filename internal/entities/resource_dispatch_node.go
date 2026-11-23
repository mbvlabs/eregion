package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
)

func (tx *StoreTx) ResourceDispatchNode(ctx context.Context, id contract.OperationID) (PackPlacement, bool, error) {
	var node PackPlacement
	err := tx.tx.QueryRow(ctx, `SELECT node_name, node_identity FROM eregion_resource_dispatch_nodes WHERE operation_id=$1`, id).Scan(&node.Node, &node.Identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return node, false, nil
	}
	return node, err == nil, err
}

func (tx *StoreTx) InsertResourceDispatchNode(ctx context.Context, id contract.OperationID, node PackPlacement) error {
	_, err := tx.tx.Exec(ctx, `INSERT INTO eregion_resource_dispatch_nodes (operation_id,node_name,node_identity) VALUES ($1,$2,$3)`, id, node.Node, node.Identity)
	return err
}
