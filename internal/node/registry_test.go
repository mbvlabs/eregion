package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestRegistryDispatchesByStepName(t *testing.T) {
	registry := DefaultRegistry()
	exec := execution(t, DiagnosticStepID(2), 1)
	payload, err := json.Marshal(DiagnosticPayload{Index: 2, Total: 3})
	if err != nil {
		t.Fatal(err)
	}
	step, err := registry.Build(exec, payload)
	if err != nil {
		t.Fatal(err)
	}
	if step.ID() != DiagnosticStepID(2) {
		t.Fatalf("built step %q", step.ID())
	}
	if step.CancelBoundary() != engine.BoundaryAfterApply {
		t.Fatal("diagnostic step must declare the after_apply boundary")
	}
}

func TestRegistryRejectsUnknownStep(t *testing.T) {
	registry := DefaultRegistry()
	exec := execution(t, "unknown-step-v1", 1)
	if _, err := registry.Build(exec, nil); err == nil {
		t.Fatal("unknown step was accepted")
	}
}

func TestRegistryRejectsPayloadThatDescribesAnotherStep(t *testing.T) {
	registry := DefaultRegistry()
	exec := execution(t, DiagnosticStepID(1), 1)
	payload, err := json.Marshal(DiagnosticPayload{Index: 2, Total: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Build(exec, payload); err == nil {
		t.Fatal("a payload for another step was accepted")
	}
}

func TestRegistryRejectsUnknownPayloadFields(t *testing.T) {
	registry := DefaultRegistry()
	exec := execution(t, DiagnosticStepID(1), 1)
	if _, err := registry.Build(exec, json.RawMessage(`{"index":1,"total":1,"extra":true}`)); err == nil {
		t.Fatal("an unknown payload field was accepted")
	}
}

func TestDispatcherBuildsStepsThroughTheRegistry(t *testing.T) {
	local, err := NewLocal(t.TempDir(), "local", nil)
	if err != nil {
		t.Fatal(err)
	}
	exec := execution(t, "not-a-diagnostic-step-v1", 1)
	response, err := local.Call(context.Background(), Request{Method: MethodRun, RequestID: "unknown", Execution: exec}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != engine.StatusFailed {
		t.Fatalf("unknown step did not fail closed: %+v", response)
	}
	if response.ErrorCode != string(contract.PreconditionFailed) {
		t.Fatalf("unknown step error code: %q", response.ErrorCode)
	}
}

func TestStepIDPatternIsVersioned(t *testing.T) {
	if !strings.HasPrefix(DiagnosticStepID(1), "diagnostic-step-") {
		t.Fatal("diagnostic step IDs must be versioned and prefixed")
	}
	if !engine.IDPattern.MatchString(DiagnosticStepID(20)) {
		t.Fatal("diagnostic step ID does not match the engine ID pattern")
	}
}
