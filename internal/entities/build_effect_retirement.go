package entities

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
)

// RecordBuildEffectRetirement records a node-confirmed retirement. The caller
// holds the operation lock and has compared the exact original dispatch.
func (tx *StoreTx) RecordBuildEffectRetirement(ctx context.Context, id contract.OperationID, step, executionID, inputsHash string) error {
	_, err := tx.tx.Exec(ctx, `INSERT INTO eregion_retired_build_effects
		(operation_id, step, execution_id, inputs_hash) VALUES ($1, $2, $3, $4)`,
		id, step, executionID, inputsHash)
	return err
}

func (tx *StoreTx) BuildEffectRetired(ctx context.Context, record StepRecord) (bool, error) {
	var retired bool
	err := tx.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM eregion_retired_build_effects
		WHERE operation_id=$1 AND step=$2 AND execution_id=$3 AND inputs_hash=$4)`,
		record.OperationID, record.Step, record.ExecutionID, record.InputsHash).Scan(&retired)
	return retired, err
}
