package operations

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/mbvlabs/eregion/internal/contract"
)

const TestKind = "diagnostic.test"
const TestTarget = "control-plane"
const TestMaxAttempts = 3

// TestInput controls bounded, database-only diagnostic work. No user text or secret
// material is accepted. Failure injection exercises retries without external effects.
type TestInput struct {
	Steps            int `json:"steps"`
	StepDelayMS      int `json:"step_delay_ms"`
	FailUntilAttempt int `json:"fail_until_attempt"`
}

type TestPreview struct {
	Kind   string    `json:"kind"`
	Target string    `json:"target"`
	Input  TestInput `json:"input"`
	DryRun bool      `json:"dry_run"`
}

func NormalizeTestInput(raw json.RawMessage) (TestInput, error) {
	input := TestInput{Steps: 3, StepDelayMS: 100}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return input, contract.NewFault(contract.ValidationFailed, "diagnostic input must be an object", "Use steps, step_delay_ms, and fail_until_attempt.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, contract.NewFault(contract.ValidationFailed, "invalid diagnostic input", "Use integer diagnostic parameters only; unknown fields are rejected.")
	}
	if decoder.Decode(new(any)) != io.EOF || input.Steps < 1 || input.Steps > 20 || input.StepDelayMS < 0 || input.StepDelayMS > 1000 || input.FailUntilAttempt < 0 || input.FailUntilAttempt > TestMaxAttempts {
		return input, contract.NewFault(contract.ValidationFailed, "diagnostic parameters out of range", "Use 1–20 steps, 0–1000 ms per step, and 0–3 failing attempts.")
	}
	return input, nil
}
