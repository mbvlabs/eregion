package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
)

type BuildStopPlacement struct {
	Node, Identity string
}

func (s *Store) BuildStopPlacement(ctx context.Context, id contract.OperationID) (BuildStopPlacement, bool, error) {
	return readBuildStopPlacement(ctx, query.New(s.pool), id)
}

func (tx *StoreTx) BuildStopPlacement(ctx context.Context, id contract.OperationID) (BuildStopPlacement, bool, error) {
	return readBuildStopPlacement(ctx, tx.q, id)
}

func readBuildStopPlacement(ctx context.Context, q *query.Queries, id contract.OperationID) (BuildStopPlacement, bool, error) {
	row, err := q.GetBuildStopPlacement(ctx, string(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BuildStopPlacement{}, false, nil
	}
	return BuildStopPlacement{Node: row.NodeName, Identity: row.NodeIdentity}, err == nil, err
}

func (tx *StoreTx) SetBuildStopPlacement(ctx context.Context, id contract.OperationID, placement BuildStopPlacement) error {
	_, err := tx.q.InsertBuildStopPlacement(ctx, query.InsertBuildStopPlacementParams{
		OperationID: string(id), NodeName: placement.Node, NodeIdentity: placement.Identity,
	})
	if err != nil {
		return err
	}
	current, found, err := readBuildStopPlacement(ctx, tx.q, id)
	if err != nil {
		return err
	}
	if !found || current != placement {
		return errors.New("build stop placement changed")
	}
	return nil
}
