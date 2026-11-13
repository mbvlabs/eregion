package controlplane

import (
	"testing"

	"github.com/pressly/goose/v3/database"
)

func TestValidateGooseHistory(t *testing.T) {
	row := func(version int64, applied bool) *database.ListMigrationsResult {
		return &database.ListMigrationsResult{Version: version, IsApplied: applied}
	}
	for _, tt := range []struct {
		name    string
		history []*database.ListMigrationsResult
		want    int
		bad     bool
	}{
		{"initial", []*database.ListMigrationsResult{row(0, true)}, 0, false},
		{"applied", []*database.ListMigrationsResult{row(1, true), row(0, true)}, 1, false},
		{"empty", nil, 0, true},
		{"unknown", []*database.ListMigrationsResult{row(2, true), row(0, true)}, 0, true},
		{"missing initial", []*database.ListMigrationsResult{row(1, true)}, 0, true},
		{"duplicate", []*database.ListMigrationsResult{row(1, true), row(1, true), row(0, true)}, 0, true},
		{"out of order", []*database.ListMigrationsResult{row(0, true), row(1, true)}, 0, true},
		{"down", []*database.ListMigrationsResult{row(1, false), row(0, true)}, 0, true},
		{"nil row", []*database.ListMigrationsResult{nil, row(0, true)}, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateGooseHistory(tt.history, []int64{1})
			if (err != nil) != tt.bad || (!tt.bad && got != tt.want) {
				t.Fatalf("got version %d, error %v", got, err)
			}
		})
	}
}
