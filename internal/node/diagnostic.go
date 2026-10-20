package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

// DiagnosticPayload identifies one bounded diagnostic step. The values come
// from the accepted operation input, never from free-form text.
type DiagnosticPayload struct {
	Index       int `json:"index"`
	Total       int `json:"total"`
	StepDelayMS int `json:"step_delay_ms"`
}

// DiagnosticStep is the harmless node effect: it creates one marker file under
// the node's private state directory. It does not change services, packages,
// network state, or application data.
type DiagnosticStep struct {
	exec    engine.Execution
	payload DiagnosticPayload
}

func NewDiagnosticStep(exec engine.Execution, payload DiagnosticPayload) (*DiagnosticStep, error) {
	if payload.Total < 1 || payload.Total > 20 || payload.Index < 1 || payload.Index > payload.Total || payload.StepDelayMS < 0 || payload.StepDelayMS > 1000 {
		return nil, fmt.Errorf("diagnostic step parameters out of range")
	}
	return &DiagnosticStep{exec: exec, payload: payload}, nil
}

func DiagnosticStepID(index int) string { return fmt.Sprintf("diagnostic-step-%02d-v1", index) }

func (s *DiagnosticStep) ID() string { return DiagnosticStepID(s.payload.Index) }

func (s *DiagnosticStep) Describe() string {
	return fmt.Sprintf("Write node-local diagnostic marker %d of %d", s.payload.Index, s.payload.Total)
}

func (s *DiagnosticStep) Inputs() ([]byte, error) {
	return json.Marshal(struct {
		OperationID string `json:"operation_id"`
		Step        string `json:"step"`
		Index       int    `json:"index"`
		Total       int    `json:"total"`
		StepDelayMS int    `json:"step_delay_ms"`
	}{
		OperationID: string(s.exec.OperationID),
		Step:        s.ID(),
		Index:       s.payload.Index,
		Total:       s.payload.Total,
		StepDelayMS: s.payload.StepDelayMS,
	})
}

func (s *DiagnosticStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

type marker struct {
	OperationID    string `json:"operation_id"`
	Step           string `json:"step"`
	InputsHash     string `json:"inputs_hash"`
	AppliedAttempt uint32 `json:"applied_attempt"`
	ExecutionID    string `json:"execution_id"`
	CreatedAt      string `json:"created_at"`
}

// MarkerPath is the durable create-once effect for one diagnostic step.
func MarkerPath(root string, exec engine.Execution) string {
	return filepath.Join(root, "effects", string(exec.OperationID), exec.Step+".applied")
}

func (s *DiagnosticStep) Check(_ context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	raw, err := os.ReadFile(MarkerPath(rt.Root, s.exec)) //nolint:gosec // G304: path is derived from a validated operation ID and step ID.
	if err != nil {
		if os.IsNotExist(err) {
			return engine.CheckResult{Complete: false, Detail: "diagnostic marker is absent"}, nil
		}
		return engine.CheckResult{}, err
	}
	var value marker
	if err := json.Unmarshal(raw, &value); err != nil {
		return engine.CheckResult{}, fmt.Errorf("%w: diagnostic marker is corrupt: %w", engine.ErrPermanent, err)
	}
	inputs, err := s.Inputs()
	if err != nil {
		return engine.CheckResult{}, err
	}
	if value.InputsHash != engine.InputsHash(s.ID(), inputs) {
		return engine.CheckResult{}, fmt.Errorf("%w: %w: diagnostic marker %s belongs to different inputs", engine.ErrPermanent, engine.ErrInputsMismatch, s.ID())
	}
	if value.OperationID != string(s.exec.OperationID) || value.Step != s.ID() {
		return engine.CheckResult{}, fmt.Errorf("%w: diagnostic marker %s has the wrong identity", engine.ErrPermanent, s.ID())
	}
	return engine.CheckResult{Complete: true, Detail: "diagnostic marker already present"}, nil
}

func (s *DiagnosticStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if s.payload.StepDelayMS > 0 {
		timer := time.NewTimer(time.Duration(s.payload.StepDelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			// The delay is before the effect; stopping here leaves no marker.
			return engine.ApplyResult{}, engine.ErrInterrupted
		case <-timer.C:
		}
	}
	inputs, err := s.Inputs()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	path := MarkerPath(rt.Root, s.exec)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return engine.ApplyResult{}, err
	}
	value := marker{
		OperationID:    string(s.exec.OperationID),
		Step:           s.ID(),
		InputsHash:     engine.InputsHash(s.ID(), inputs),
		AppliedAttempt: s.exec.Attempt,
		ExecutionID:    s.exec.ExecutionID(),
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return engine.ApplyResult{}, err
	}
	existed, err := engine.WriteFileOnce(path, append(raw, '\n'), 0o600)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if existed {
		// The marker is create-once. Accept only an identical prior effect; never
		// overwrite the record that says which attempt actually applied it.
		existing, err := os.ReadFile(path) //nolint:gosec // G304: same validated effect path.
		if err != nil {
			return engine.ApplyResult{}, err
		}
		var prior marker
		if err := json.Unmarshal(existing, &prior); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: diagnostic marker is corrupt: %w", engine.ErrPermanent, err)
		}
		if prior.InputsHash != value.InputsHash || prior.OperationID != value.OperationID || prior.Step != value.Step {
			return engine.ApplyResult{}, fmt.Errorf("%w: %w: diagnostic marker %s was already applied with different inputs", engine.ErrPermanent, engine.ErrInputsMismatch, s.ID())
		}
		if report != nil {
			report(engine.Event{Kind: "log", Step: s.ID(), Message: "diagnostic marker already existed; no second effect"})
		}
		return engine.ApplyResult{Result: map[string]any{"applied": false, "applied_attempt": prior.AppliedAttempt}}, nil
	}
	return engine.ApplyResult{Result: map[string]any{"applied": true, "applied_attempt": s.exec.Attempt}}, nil
}
