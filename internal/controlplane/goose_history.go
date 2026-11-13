package controlplane

import (
	"fmt"

	"github.com/pressly/goose/v3/database"
)

// validateGooseHistory validates the entire ledger in the descending insertion
// order returned by Goose, rather than treating the maximum version as proof of
// a complete, unmodified migration history.
func validateGooseHistory(history []*database.ListMigrationsResult, versions []int64) (int, error) {
	if len(history) == 0 || history[len(history)-1] == nil ||
		history[len(history)-1].Version != 0 || !history[len(history)-1].IsApplied {
		return 0, fmt.Errorf("missing Goose initial version")
	}
	if len(history)-1 > len(versions) {
		return 0, fmt.Errorf("unknown or duplicate Goose migration")
	}
	for i := 0; i < len(history)-1; i++ {
		row := history[len(history)-2-i]
		if row == nil || !row.IsApplied || row.Version != versions[i] {
			return 0, fmt.Errorf("invalid or out-of-order Goose migration at position %d", i+1)
		}
	}
	if len(history) == 1 {
		return 0, nil
	}
	return int(history[0].Version), nil
}
