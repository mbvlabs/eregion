package controlplane

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/transport"
)

// TestJobTimeoutBoundsTheNodeTaskWait proves that the transport's own wait
// bound is the visible one. The River job timeout is the real deadline for a
// worker, so the transport's default stays below it: a worker waits at most
// until its job deadline, and never longer than the documented bound.
func TestJobTimeoutBoundsTheNodeTaskWait(t *testing.T) {
	if transport.DefaultMaxWait >= jobTimeout {
		t.Fatalf("the default node task wait %s is not below the job timeout %s", transport.DefaultMaxWait, jobTimeout)
	}
}
