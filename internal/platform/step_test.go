package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestContainerStepAcceptsBareAndPrefixedLabels(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-container-01-v1"}
	payload := ContainerPayload{
		Step: "platform-container-01-v1", Name: "eregion-test-candidate", Image: "sha256:" + strings.Repeat("b", 64),
		ContainerPort: 8080, HostPort: 30000, Bind: "127.0.0.1",
		Labels: map[string]string{
			"io.eregion.app": "api", "io.eregion.environment": "production",
			"io.eregion.release": "rel_" + strings.Repeat("c", 32), "io.eregion.role": "candidate",
			"com.example/team": "platform",
		},
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err != nil {
		t.Fatalf("valid labels were refused: %v", err)
	}
	payload.Labels = map[string]string{"bad label": "x"}
	raw, _ = json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err == nil {
		t.Fatal("a label with whitespace was accepted")
	}
	payload.Labels = map[string]string{"bad_prefix/name": "x"}
	raw, _ = json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err == nil {
		t.Fatal("an invalid DNS prefix was accepted")
	}
}
