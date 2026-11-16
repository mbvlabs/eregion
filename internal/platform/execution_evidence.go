package platform

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/output"
)

// ExecutionEvidence is an observation of the original container, before an
// existing cleanup path removes it. It never grants authority to remove it.
type ExecutionEvidence struct {
	Availability string    `json:"availability"`
	ContainerID  string    `json:"container_id,omitempty"`
	ObservedAt   time.Time `json:"observed_at,omitempty"`
	State        string    `json:"state,omitempty"`
	Running      bool      `json:"running"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	ExitCode     *int      `json:"exit_code,omitempty"`
	OOMKilled    bool      `json:"oom_killed"`
	Health       string    `json:"health,omitempty"`
	Logs         string    `json:"logs,omitempty"`
	Truncated    bool      `json:"truncated"`
}

// StepExecutionEvidence is carried in a node response's result on failure.
// On success it is nested in the ordinary step result when replacement was
// observed. Each response is scoped by its node execution ID and attempt.
type StepExecutionEvidence struct {
	Stage              string             `json:"stage"`
	OutputAvailability string             `json:"output_availability"`
	RedactionContext   string             `json:"redaction_context"`
	StdoutTruncated    bool               `json:"stdout_truncated"`
	StderrTruncated    bool               `json:"stderr_truncated"`
	Stdout             string             `json:"stdout,omitempty"`
	Stderr             string             `json:"stderr,omitempty"`
	Previous           *ExecutionEvidence `json:"previous,omitempty"`
	Container          *ExecutionEvidence `json:"container,omitempty"`
}

func originalRedactionContext(rt engine.Runtime) string {
	if len(rt.SecretValues) == 0 {
		return "not_required"
	}
	if rt.Secrets == nil {
		return "unavailable"
	}
	return "original"
}

var evidenceContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func captureExecutionEvidence(parent context.Context, name string, labels map[string]string, rt engine.Runtime) ExecutionEvidence {
	result := ExecutionEvidence{Availability: "unavailable"}
	if !executionEvidenceLabelsComplete(labels) || rt.Secrets == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	inspection, ok := readExecutionInspection(ctx, name)
	if !ok || !executionLabelsMatch(labels, inspection.Labels) || !knownExecutionState(inspection.State.Status) {
		return result
	}
	result = executionEvidenceFromInspection(inspection)
	if rt.Secrets.RequiresWholeTextRedaction() {
		// A Docker tail can begin within a secret. Metadata remains useful,
		// but no bounded tail can prove the missing prefix was safe.
		return result
	}
	return captureEvidenceLogs(ctx, inspection.ID, rt, result)
}

// executionEvidenceLabelsComplete enforces the existing deployment ownership
// boundary. Empty or caller-supplied partial labels are not evidence of an
// owned execution.
func executionEvidenceLabelsComplete(labels map[string]string) bool {
	for _, key := range []string{"io.eregion.app", "io.eregion.environment", "io.eregion.process.kind", "io.eregion.release", "io.eregion.deployment"} {
		if labels[key] == "" {
			return false
		}
	}
	return true
}

type executionContainerState struct {
	Status     string
	Running    bool
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	OOMKilled  bool
	Health     *struct{ Status string }
}

type executionInspection struct {
	ID     string
	Labels map[string]string
	State  executionContainerState
}

func readExecutionInspection(ctx context.Context, name string) (executionInspection, bool) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type=container", "--format", "{{json .Id}}\n{{json .Config.Labels}}\n{{json .State}}", name)
	buf := &limitedDiagnosticBuffer{limit: 64 * 1024}
	cmd.Stdout = buf
	if cmd.Run() != nil || buf.overflow {
		return executionInspection{}, false
	}
	parts := strings.SplitN(buf.String(), "\n", 3)
	var inspection executionInspection
	if len(parts) != 3 ||
		json.Unmarshal([]byte(parts[0]), &inspection.ID) != nil ||
		!evidenceContainerID.MatchString(inspection.ID) ||
		json.Unmarshal([]byte(parts[1]), &inspection.Labels) != nil ||
		json.Unmarshal([]byte(parts[2]), &inspection.State) != nil {
		return executionInspection{}, false
	}
	return inspection, true
}

func executionLabelsMatch(labels, actual map[string]string) bool {
	for key, value := range labels {
		if actual[key] != value {
			return false
		}
	}
	return true
}

func knownExecutionState(status string) bool {
	switch status {
	case "created", "running", "paused", "restarting", "removing", "exited", "dead":
		return true
	default:
		return false
	}
}

func executionEvidenceFromInspection(inspection executionInspection) ExecutionEvidence {
	state := inspection.State
	result := ExecutionEvidence{Availability: "partial", ContainerID: inspection.ID, ObservedAt: time.Now().UTC(), State: state.Status, Running: state.Running, StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, OOMKilled: state.OOMKilled}
	if !state.StartedAt.IsZero() && !state.FinishedAt.IsZero() {
		result.ExitCode = &state.ExitCode
	}
	if state.Health != nil {
		switch state.Health.Status {
		case "starting", "healthy", "unhealthy":
			result.Health = state.Health.Status
		}
	}
	return result
}

func captureEvidenceLogs(ctx context.Context, id string, rt engine.Runtime, result ExecutionEvidence) ExecutionEvidence {
	logs := exec.CommandContext(ctx, "docker", "logs", "--tail", "201", id)
	raw := &limitedDiagnosticBuffer{limit: 1024 * 1024}
	logs.Stdout, logs.Stderr = raw, raw
	if logs.Run() != nil || raw.overflow {
		result.Truncated = raw.overflow
		return result
	}
	// Docker's tail is bounded by lines and may have omitted earlier output,
	// so even a successful read is not a claim of complete capture.
	text := rt.Secrets.Redact(raw.String())
	text = rt.Secrets.Redact(output.SafeText(text))
	if len(text) > 32*1024 {
		text = diagnosticPrefix(text, 32*1024)
		result.Truncated = true
	}
	if strings.Count(text, "\n") >= 200 {
		result.Truncated = true
	}
	result.Logs = text
	result.Availability = "partial"
	return result
}
