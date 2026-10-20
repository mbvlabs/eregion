package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

// StepFactory builds the step for one execution identity and request payload.
// The payload is bounded, validated data, never free-form text.
type StepFactory func(exec engine.Execution, payload json.RawMessage) (engine.Step, error)

// StepRegistry maps a versioned step ID to the step that implements it. The
// node binary dispatches by step name through it, and the control plane builds
// the same step through the same registry, so both sides agree on the
// canonical inputs of an effect.
//
// A registry is a small, ordered list of step families. The first family whose
// matcher accepts the step ID builds the step.
type StepRegistry struct {
	families []stepFamily
}

type stepFamily struct {
	match func(stepID string) bool
	build StepFactory
}

// diagnosticStepPattern matches the versioned diagnostic step family.
var diagnosticStepPattern = regexp.MustCompile(`^diagnostic-step-[0-9]{2}-v1$`)

// NewStepRegistry returns a registry with the built-in diagnostic and platform
// step families. The platform steps are the only path a release or deployment
// uses to reach Docker or Buildpacks, so the control plane and node binary derive
// identical inputs and effect identities.
func NewStepRegistry() *StepRegistry { return NewStepRegistryWithHost(HostSettings{}) }

func newBaseRegistry() *StepRegistry {
	registry := &StepRegistry{}
	registry.Register(diagnosticStepPattern.MatchString, buildDiagnosticStep)
	for _, family := range platform.StepFamilies() {
		registry.Register(family.Match, family.Build)
	}
	return registry
}

// DefaultRegistry returns the registry both sides of the transport use.
func DefaultRegistry() *StepRegistry { return NewStepRegistry() }

// Register adds a step family. Later families never shadow earlier ones.
func (r *StepRegistry) Register(match func(stepID string) bool, build StepFactory) {
	if match == nil || build == nil {
		return
	}
	r.families = append(r.families, stepFamily{match: match, build: build})
}

// Build returns the step for an execution identity. An unknown step is an
// explicit failure: the node never guesses what a step meant.
func (r *StepRegistry) Build(exec engine.Execution, payload json.RawMessage) (engine.Step, error) {
	for _, family := range r.families {
		if family.match(exec.Step) {
			return family.build(exec, payload)
		}
	}
	return nil, fmt.Errorf("unknown step %q", exec.Step)
}

// buildDiagnosticStep builds the harmless node-local diagnostic step and
// checks that the requested step ID is the one the payload describes.
func buildDiagnosticStep(exec engine.Execution, payload json.RawMessage) (engine.Step, error) {
	if len(payload) == 0 {
		return nil, errors.New("diagnostic step payload is required")
	}
	var value DiagnosticPayload
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid diagnostic step payload: %w", err)
	}
	step, err := NewDiagnosticStep(exec, value)
	if err != nil {
		return nil, err
	}
	if step.ID() != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, step.ID())
	}
	return step, nil
}
