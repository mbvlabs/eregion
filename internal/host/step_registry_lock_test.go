package host

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

// TestRegistryStepHoldsTheHostLock covers the host step that pulls the pinned
// registry image and restarts the registry service. It must stay inside the
// shared host lock like every other node mutation; the retention garbage-
// collection step relies on that when it stops and restarts the same unit.
func TestRegistryStepHoldsTheHostLock(t *testing.T) {
	step := &registryStep{}
	if independent, ok := interface{}(step).(engine.HostLockIndependent); ok && independent.IndependentOfHostLock() {
		t.Fatal("host-registry-v2 opts out of the host lock but pulls and restarts the registry image")
	}
}
