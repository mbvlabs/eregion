package entities

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
)

type DeploymentHealthDispatch struct {
	Placement   PackPlacement
	Attempt     uint32
	ExecutionID string
	InputsHash  string
}

func (tx *StoreTx) DeploymentHealthDispatch(ctx context.Context, id contract.OperationID, step string) (DeploymentHealthDispatch, bool, error) {
	var d DeploymentHealthDispatch
	err := tx.tx.QueryRow(ctx, `SELECT node_name,node_identity,attempt,execution_id,inputs_hash FROM eregion_deployment_health_dispatches WHERE operation_id=$1 AND step=$2`, id, step).Scan(&d.Placement.Node, &d.Placement.Identity, &d.Attempt, &d.ExecutionID, &d.InputsHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}

func (tx *StoreTx) InsertDeploymentHealthDispatch(ctx context.Context, id contract.OperationID, step string, d DeploymentHealthDispatch) error {
	_, err := tx.tx.Exec(ctx, `INSERT INTO eregion_deployment_health_dispatches (operation_id,step,node_name,node_identity,attempt,execution_id,inputs_hash) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, step, d.Placement.Node, d.Placement.Identity, d.Attempt, d.ExecutionID, d.InputsHash)
	return err
}
