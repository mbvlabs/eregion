package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/retention"
)

// TestImageMovingStepsHoldTheHostLock covers every platform step family that
// can pull, tag, load, untag or delete a local Docker image. The retention
// untag is only safe against a concurrent tag move because the engine's host
// lock serializes it with pack builds, deploy pulls, image pushes, container
// and release-command runs, and registry garbage collection. A new image-moving
// step family must not opt out of that lock.
func TestImageMovingStepsHoldTheHostLock(t *testing.T) {
	rawPack, _ := json.Marshal(PackPayload{
		Step: "platform-pack-01-v1", Image: "eregion/api-production:abc",
		Builder: DefaultBuilder, Path: t.TempDir(),
	})
	pack, err := buildPackStep(retentionTestExecution("platform-pack-01-v1"), rawPack)
	if err != nil {
		t.Fatal(err)
	}

	rawPull, _ := json.Marshal(ArgvPayload{
		Step: "platform-pull-01-v1", Argv: []string{"docker", "pull", "127.0.0.1:5000/api:new"},
		TimeoutSeconds: 600,
	})
	pull, err := buildArgvStep(retentionTestExecution("platform-pull-01-v1"), rawPull)
	if err != nil {
		t.Fatal(err)
	}

	rawContainer, _ := json.Marshal(ContainerPayload{
		Step: "platform-container-web-1-v1", Name: "eregion-api-production-web-1",
		Image: "127.0.0.1:5000/api:new", Command: []string{"echo", "ready"},
	})
	container, err := buildContainerStep(retentionTestExecution("platform-container-web-1-v1"), rawContainer)
	if err != nil {
		t.Fatal(err)
	}

	rawCommand, _ := json.Marshal(ReleaseCommandPayload{
		Step: "platform-command-01-v1", Name: "eregion-api-production-command",
		Image: "127.0.0.1:5000/api:new", Network: "eregion-api-production",
		Command: []string{"./migrate"},
	})
	command, err := buildReleaseCommandStep(retentionTestExecution("platform-command-01-v1"), rawCommand)
	if err != nil {
		t.Fatal(err)
	}

	remove := retentionRemoveStepFor(t, retention.ItemImage, "sha256:"+strings.Repeat("a", 64), nil)
	gc, err := buildRetentionGCStep(retentionTestExecution("platform-retention-gc-01-v1"), []byte(`{"step":"platform-retention-gc-01-v1"}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []engine.Step{pack, pull, container, command, remove, gc} {
		if independent, ok := step.(engine.HostLockIndependent); ok && independent.IndependentOfHostLock() {
			t.Fatalf("%s opts out of the host lock but can move a local image", step.ID())
		}
	}
}
