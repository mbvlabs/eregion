package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

type PackPlacement struct {
	Node, Identity string
}

func (s *Store) BuildPlacement(ctx context.Context, id contract.OperationID) (PackPlacement, bool, error) {
	return readBuildPlacement(ctx, query.New(s.pool), id)
}

func (tx *StoreTx) BuildPlacement(ctx context.Context, id contract.OperationID) (PackPlacement, bool, error) {
	return readBuildPlacement(ctx, tx.q, id)
}

func readBuildPlacement(ctx context.Context, q *query.Queries, id contract.OperationID) (PackPlacement, bool, error) {
	row, err := q.GetBuildPlacement(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return PackPlacement{}, false, nil
	}
	return PackPlacement{Node: row.NodeName, Identity: row.NodeIdentity}, err == nil, err
}

func (tx *StoreTx) InsertBuildPlacement(ctx context.Context, id contract.OperationID, placement PackPlacement) error {
	return tx.q.InsertBuildPlacement(ctx, query.InsertBuildPlacementParams{
		OperationID: string(id), NodeName: placement.Node, NodeIdentity: placement.Identity,
	})
}
