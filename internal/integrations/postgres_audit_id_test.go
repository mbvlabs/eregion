package integrations

import (
	"math"
	"testing"
)

func TestNextAuditID(t *testing.T) {
	for _, tc := range []struct {
		last, want int64
		valid      bool
	}{{0, 1, true}, {41, 42, true}, {-1, 0, false}, {math.MaxInt64, 0, false}} {
		got, err := nextAuditID(tc.last)
		if got != tc.want || (err == nil) != tc.valid {
			t.Fatalf("nextAuditID(%d) = %d, %v", tc.last, got, err)
		}
	}
}
