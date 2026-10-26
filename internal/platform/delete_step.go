package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

// deleteStepPattern matches the versioned workload-cleanup step used before an
// environment or application row is removed.
var deleteStepPattern = regexp.MustCompile(`^platform-delete-[0-9]{2}-v1$`)

// DeletePayload selects every managed container for an application, or for one
// environment when Environment is set. Selection is by ownership labels.
type DeletePayload struct {
	Step        string `json:"step"`
	App         string `json:"app"`
	Environment string `json:"environment,omitempty"`
}

type DeleteStep struct {
	exec    engine.Execution
	payload DeletePayload
}

func buildDeleteStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload DeletePayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if err := ValidateName(payload.App); err != nil {
		return nil, err
	}
	if payload.Environment != "" {
		if err := ValidateName(payload.Environment); err != nil {
			return nil, err
		}
	}
	return &DeleteStep{exec: exec, payload: payload}, nil
}

func (s *DeleteStep) ID() string { return s.payload.Step }
func (s *DeleteStep) Describe() string {
	return "Remove managed workload containers selected by ownership labels"
}
func (s *DeleteStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *DeleteStep) Inputs() ([]byte, error) { return json.Marshal(s.payload) }

func (s *DeleteStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, _, err := runArgv(probeCtx, workloadContainerListArgs(s.payload.App, s.payload.Environment), nil, rt, s.ID(), nil)
	if err != nil {
		return engine.CheckResult{Complete: false, Detail: "workload containers could not be listed"}, nil
	}
	if strings.TrimSpace(stdout) == "" {
		return engine.CheckResult{Complete: true, Detail: "no managed workload containers remain"}, nil
	}
	return engine.CheckResult{Complete: false, Detail: "managed workload containers still exist"}, nil
}

func (s *DeleteStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(runCtx, workloadContainerListArgs(s.payload.App, s.payload.Environment), nil, rt, s.ID(), report)
	if err != nil {
		return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("list workload containers: %w: %s", err, firstLine(stderr)))
	}
	identifiers := strings.Fields(stdout)
	if len(identifiers) == 0 {
		return engine.ApplyResult{Result: map[string]any{"removed": 0}}, nil
	}
	argv := append([]string{"docker", "rm", "--force"}, identifiers...)
	_, rmStderr, err := runArgv(runCtx, argv, nil, rt, s.ID(), report)
	if err != nil {
		return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("remove workload containers: %w: %s", err, firstLine(rmStderr)))
	}
	return engine.ApplyResult{Result: map[string]any{"removed": len(identifiers)}}, nil
}

// workloadContainerListArgs builds the read-only Docker selector. It is pure and
// shared by Check and Apply so both act on exactly the same label set.
func workloadContainerListArgs(app, environment string) []string {
	args := []string{"docker", "ps", "--all", "--quiet",
		"--filter", "label=io.eregion.app=" + app}
	if environment != "" {
		args = append(args, "--filter", "label=io.eregion.environment="+environment)
	}
	return args
}

var _ engine.Step = (*DeleteStep)(nil)
