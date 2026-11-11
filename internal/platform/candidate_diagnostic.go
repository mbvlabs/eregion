package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/output"
)

const CandidateDiagnosticStage = "candidate-diagnostic"
const CandidateDiagnosticStep = "platform-candidate-diagnostic-01-v1"

var candidateDiagnosticStepPattern = regexp.MustCompile(`^platform-candidate-diagnostic-01-v1$`)

type CandidateDiagnosticContainer struct {
	Name    string `json:"name"`
	Process string `json:"process"`
	Kind    string `json:"kind"`
	Replica string `json:"replica"`
}

type CandidateDiagnosticPayload struct {
	Step         string                         `json:"step"`
	App          string                         `json:"app"`
	Environment  string                         `json:"environment"`
	ReleaseID    ReleaseID                      `json:"release_id"`
	DeploymentID DeploymentID                   `json:"deployment_id"`
	Deadline     time.Time                      `json:"deadline"`
	Containers   []CandidateDiagnosticContainer `json:"containers"`
}

type CandidateDiagnosticLine struct {
	Process     string    `json:"process"`
	Replica     string    `json:"replica"`
	Container   string    `json:"container"`
	ContainerID string    `json:"container_id"`
	Timestamp   time.Time `json:"timestamp"`
	Message     string    `json:"message"`
}

type CandidateDiagnosticResult struct {
	Lines       []CandidateDiagnosticLine `json:"lines"`
	Truncated   bool                      `json:"truncated"`
	Unavailable bool                      `json:"unavailable"`
}

type candidateDiagnosticStep struct{ payload CandidateDiagnosticPayload }

func buildCandidateDiagnosticStep(execution engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var p CandidateDiagnosticPayload
	if err := decodePayload(execution, raw, &p); err != nil {
		return nil, err
	}
	if p.Step != execution.Step || p.Step != CandidateDiagnosticStep || ValidateName(p.App) != nil || ValidateName(p.Environment) != nil || p.ReleaseID.Validate() != nil || p.DeploymentID.Validate() != nil || p.Deadline.IsZero() || len(p.Containers) > 64 {
		return nil, errors.New("invalid candidate diagnostic request")
	}
	seen := map[string]bool{}
	for _, c := range p.Containers {
		if !containerNamePattern.MatchString(c.Name) || !processNamePattern.MatchString(c.Process) || c.Replica == "" || c.Name != WorkloadContainerName(p.App, p.Environment, c.Process, c.Replica, p.DeploymentID) || seen[c.Name] {
			return nil, errors.New("invalid candidate identity")
		}
		seen[c.Name] = true
	}
	return &candidateDiagnosticStep{payload: p}, nil
}

func (s *candidateDiagnosticStep) ID() string { return s.payload.Step }
func (*candidateDiagnosticStep) Describe() string {
	return "Read bounded candidate container diagnostics"
}
func (*candidateDiagnosticStep) CancelBoundary() engine.Boundary { return engine.BoundaryBeforeApply }
func (s *candidateDiagnosticStep) Inputs() ([]byte, error)       { return json.Marshal(s.payload) }
func (*candidateDiagnosticStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}
func (*candidateDiagnosticStep) IndependentOfHostLock() bool { return true }

// limitedDiagnosticBuffer fails rather than retaining a prefix of an oversized
// raw stream: a secret may straddle any such prefix boundary.
type limitedDiagnosticBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedDiagnosticBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.overflow = true
		return 0, errors.New("diagnostic input limit exceeded")
	}
	return b.Buffer.Write(p)
}

func (s *candidateDiagnosticStep) Apply(ctx context.Context, rt engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, deadlineCancel := context.WithDeadline(ctx, s.payload.Deadline)
	defer deadlineCancel()
	result := CandidateDiagnosticResult{Lines: []CandidateDiagnosticLine{}}
	if rt.Secrets == nil {
		result.Unavailable = true
		return engine.ApplyResult{Result: result}, nil
	}
	// Docker timestamps are inserted between lines, preventing exact matching of
	// a multiline secret. Fail closed instead of persisting a fragment.
	for _, value := range rt.SecretValues {
		if strings.ContainsAny(value, "\r\n") {
			result.Unavailable = true
			return engine.ApplyResult{Result: result}, nil
		}
	}
	// Reserve space for the durable claim and summary messages as well.
	budget := 64*1024 - 512
	for _, c := range s.payload.Containers {
		if ctx.Err() != nil {
			result.Unavailable = true
			break
		}
		if budget <= 0 {
			result.Truncated = true
			break
		}
		inspect := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .Id}}\n{{json .Name}}\n{{json .Config.Labels}}", c.Name)
		metadata := &limitedDiagnosticBuffer{limit: 64 * 1024}
		inspect.Stdout = metadata
		if err := inspect.Run(); err != nil || metadata.overflow {
			result.Unavailable = true
			continue
		}
		parts := strings.SplitN(metadata.String(), "\n", 3)
		var id, name string
		var labels map[string]string
		if len(parts) != 3 || json.Unmarshal([]byte(parts[0]), &id) != nil || json.Unmarshal([]byte(parts[1]), &name) != nil || json.Unmarshal([]byte(parts[2]), &labels) != nil || strings.TrimPrefix(name, "/") != c.Name || id == "" {
			result.Unavailable = true
			continue
		}
		expected := WorkloadLabels(s.payload.App, s.payload.Environment, c.Process, c.Kind, c.Replica, s.payload.ReleaseID, s.payload.DeploymentID)
		match := true
		for key, value := range expected {
			if labels[key] != value {
				match = false
				break
			}
		}
		if !match {
			result.Unavailable = true
			continue
		}
		command := exec.CommandContext(ctx, "docker", "logs", "--timestamps", "--tail", "200", id)
		raw := &limitedDiagnosticBuffer{limit: 1024 * 1024}
		command.Stdout, command.Stderr = raw, raw
		if err := command.Run(); err != nil || raw.overflow {
			result.Unavailable = true
			if raw.overflow {
				result.Truncated = true
			}
			continue
		}
		// Redact the complete raw stream before newline normalization, parsing,
		// safe-text conversion or any truncation.
		text := rt.Secrets.Redact(raw.String())
		containerLines := 0
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			if line == "" {
				continue
			}
			if len(result.Lines) >= 512 {
				result.Truncated = true
				break
			}
			if containerLines >= 200 {
				result.Truncated = true
				break
			}
			stamp, message, ok := strings.Cut(line, " ")
			if !ok {
				result.Unavailable = true
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				result.Unavailable = true
				continue
			}
			message = output.SafeText(message)
			if len(message) > 32*1024 {
				message = diagnosticPrefix(message, 32*1024)
				result.Truncated = true
			}
			if len(message) > budget {
				message = diagnosticPrefix(message, budget)
				result.Truncated = true
			}
			result.Lines = append(result.Lines, CandidateDiagnosticLine{Process: c.Process, Replica: c.Replica, Container: c.Name, ContainerID: id, Timestamp: at, Message: message})
			containerLines++
			budget -= len(message)
			if budget == 0 {
				result.Truncated = true
				break
			}
		}
		if len(result.Lines) >= 512 {
			break
		}
	}
	return engine.ApplyResult{Result: result}, nil
}

func diagnosticPrefix(text string, max int) string {
	if len(text) <= max {
		return text
	}
	for max > 0 && !utf8.RuneStart(text[max]) {
		max--
	}
	return text[:max]
}
