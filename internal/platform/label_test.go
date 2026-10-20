package platform

import (
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

// Docker accepts both reverse-DNS and namespace/name label keys. The worker
// emits io.eregion.* labels, so the validator must accept them without
// weakening the requirement for a nonempty prefix and name.
func TestContainerLabelsRequireAQualifiedPrefix(t *testing.T) {
	exec := engine.Execution{OperationID: "op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Attempt: 1, Step: "platform-container-01-v1", RequestID: "label-test"}
	payload := ContainerPayload{
		Step: exec.Step, Name: "eregion-test", Image: "registry.example/app:v1",
		ContainerPort: 80, HostPort: 8080, Bind: "127.0.0.1",
	}
	for _, key := range []string{"io.eregion.app", "io.eregion/app"} {
		payload.Labels = map[string]string{key: "value"}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildContainerStep(exec, raw); err != nil {
			t.Fatalf("qualified label %q was refused: %v", key, err)
		}
	}
	for _, key := range []string{"unqualified", ".leading", "trailing."} {
		payload.Labels = map[string]string{key: "value"}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildContainerStep(exec, raw); err == nil {
			t.Fatalf("unqualified label %q was accepted", key)
		}
	}
}
