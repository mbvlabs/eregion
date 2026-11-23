package entities

import "context"

// TargetClaimed reports whether a target is reserved, including by a sealed
// claim. Callers coordinating admission must hold the target's transaction
// advisory gate before checking, and claim acquisition must use the same gate.
func (tx *StoreTx) TargetClaimed(ctx context.Context, target string) (bool, error) {
	var claimed bool
	err := tx.tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM eregion_target_claim_targets WHERE target = $1
	)`, target).Scan(&claimed)
	return claimed, err
}
