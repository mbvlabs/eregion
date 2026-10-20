package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// PlanEntry is one step's observed state in a plan or status response.
type PlanEntry struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Complete    bool     `json:"complete"`
	Detail      string   `json:"detail,omitempty"`
	Diff        []string `json:"diff,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// Step is an engine step that can also describe its desired diff without
// applying anything.
type Step interface {
	engine.Step
	Plan(context.Context, engine.Runtime) (PlanEntry, error)
}

type differ interface {
	Diff(context.Context, engine.Runtime) ([]string, error)
}

func planEntry(ctx context.Context, step Step, runtime engine.Runtime) (entry PlanEntry, err error) {
	entry = PlanEntry{ID: step.ID(), Description: step.Describe()}
	check, checkErr := step.Check(ctx, runtime)
	entry.Complete = check.Complete
	entry.Detail = check.Detail
	if checkErr != nil {
		entry.Error = safeError(checkErr)
		return entry, nil
	}
	if !check.Complete {
		if d, ok := step.(differ); ok {
			entry.Diff, err = d.Diff(ctx, runtime)
			if err != nil {
				entry.Error = safeError(err)
			}
		}
	}
	return entry, nil
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return text
}

func canonicalInput(value any) ([]byte, error) {
	return json.Marshal(value)
}

func scriptEnv(extra map[string]string) map[string]string {
	env := map[string]string{}
	for key, value := range extra {
		if value != "" {
			env[key] = value
		}
	}
	return env
}

func incomplete(detail string) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: false, Detail: detail}, nil
}

func complete(detail string) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: true, Detail: detail}, nil
}

func checkOutput(ctx context.Context, runtime engine.Runtime, exec Exec, script string) (string, error) {
	output, err := exec.Output(ctx, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func requireValue(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func contractError(message string) error {
	return contract.NewFault(contract.PreconditionFailed, message, "Inspect the host plan and the service/database readiness details before retrying.")
}
