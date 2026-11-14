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
	// These labels are the existing deployment ownership boundary. Empty or
	// caller-supplied partial labels are not evidence of an owned execution.
	for _, key := range []string{"io.eregion.app", "io.eregion.environment", "io.eregion.process.kind", "io.eregion.release", "io.eregion.deployment"} {
		if labels[key] == "" {
			return result
		}
	}
	if rt.Secrets == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type=container", "--format", "{{json .Id}}\n{{json .Config.Labels}}\n{{json .State}}", name)
	buf := &limitedDiagnosticBuffer{limit: 64 * 1024}
	cmd.Stdout = buf
	if cmd.Run() != nil || buf.overflow {
		return result
	}
	parts := strings.SplitN(buf.String(), "\n", 3)
	var id string
	var actual map[string]string
	var state struct {
		Status     string
		Running    bool
		StartedAt  time.Time
		FinishedAt time.Time
		ExitCode   int
		OOMKilled  bool
		Health     *struct{ Status string }
	}
	if len(parts) != 3 || json.Unmarshal([]byte(parts[0]), &id) != nil || !evidenceContainerID.MatchString(id) || json.Unmarshal([]byte(parts[1]), &actual) != nil || json.Unmarshal([]byte(parts[2]), &state) != nil {
		return result
	}
	for key, value := range labels {
		if actual[key] != value {
			return result
		}
	}
	switch state.Status {
	case "created", "running", "paused", "restarting", "removing", "exited", "dead":
	default:
		return result
	}
	result = ExecutionEvidence{Availability: "partial", ContainerID: id, ObservedAt: time.Now().UTC(), State: state.Status, Running: state.Running, StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, OOMKilled: state.OOMKilled}
	if !state.StartedAt.IsZero() && !state.FinishedAt.IsZero() {
		result.ExitCode = &state.ExitCode
	}
	if state.Health != nil {
		switch state.Health.Status {
		case "starting", "healthy", "unhealthy":
			result.Health = state.Health.Status
		}
	}
	if rt.Secrets.RequiresWholeTextRedaction() {
		// A Docker tail can begin within a secret. Metadata remains useful,
		// but no bounded tail can prove the missing prefix was safe.
		return result
	}
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
