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

var candidateDiagnosticStepPattern = regexp.MustCompile(`^platform-candidate-diagnostic-[a-f0-9]+-v1$`)

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
	MetadataOnly bool                           `json:"metadata_only,omitempty"`
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
	Lines              []CandidateDiagnosticLine `json:"lines"`
	Observations       []CandidateObservation    `json:"observations"`
	LogTimestampSource string                    `json:"log_timestamp_source,omitempty"`
	RedactionContext   string                    `json:"redaction_context,omitempty"`
	Truncated          bool                      `json:"truncated"`
	Unavailable        bool                      `json:"unavailable"`
	ImageMismatch      bool                      `json:"image_mismatch,omitempty"`
}

// CandidateObservation is a point-in-time Docker observation, not an inferred
// cause. StartedAt and FinishedAt establish whether the lifecycle advanced.
type CandidateObservation struct {
	Process     string    `json:"process"`
	Replica     string    `json:"replica"`
	Container   string    `json:"container"`
	ContainerID string    `json:"container_id"`
	ObservedAt  time.Time `json:"observed_at"`
	State       string    `json:"state"`
	Running     bool      `json:"running"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	OOMKilled   bool      `json:"oom_killed"`
	Health      string    `json:"health,omitempty"`
}

type candidateDiagnosticStep struct {
	payload         CandidateDiagnosticPayload
	expectedImageID string
}

func buildCandidateDiagnosticStep(execution engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var p CandidateDiagnosticPayload
	if err := decodePayload(execution, raw, &p); err != nil {
		return nil, err
	}
	if p.Step != execution.Step {
		return nil, errors.New("invalid candidate diagnostic request")
	}
	if err := validateCandidateDiagnostic(p); err != nil {
		return nil, err
	}
	return &candidateDiagnosticStep{payload: p}, nil
}

func validateCandidateDiagnostic(p CandidateDiagnosticPayload) error {
	if !candidateDiagnosticStepPattern.MatchString(p.Step) || ValidateName(p.App) != nil || ValidateName(p.Environment) != nil || p.ReleaseID.Validate() != nil || p.DeploymentID.Validate() != nil || p.Deadline.IsZero() || len(p.Containers) > 64 {
		return errors.New("invalid candidate diagnostic request")
	}
	seen := map[string]bool{}
	for _, c := range p.Containers {
		if !containerNamePattern.MatchString(c.Name) || !processNamePattern.MatchString(c.Process) || c.Replica == "" || c.Name != WorkloadContainerName(p.App, p.Environment, c.Process, c.Replica, p.DeploymentID) || seen[c.Name] {
			return errors.New("invalid candidate identity")
		}
		seen[c.Name] = true
	}
	return nil
}

// ObserveCandidateDiagnostics performs the existing bounded, owned-label
// diagnostic read directly, without a node task or journal write.
func ObserveCandidateDiagnostics(ctx context.Context, p CandidateDiagnosticPayload, expectedImageID string, rt engine.Runtime) (CandidateDiagnosticResult, error) {
	if err := validateCandidateDiagnostic(p); err != nil {
		return CandidateDiagnosticResult{}, err
	}
	if !diagnosisImageIDPattern.MatchString(expectedImageID) {
		return CandidateDiagnosticResult{}, errors.New("invalid candidate image identity")
	}
	result, err := (&candidateDiagnosticStep{payload: p, expectedImageID: expectedImageID}).Apply(ctx, rt, nil)
	if err != nil {
		return CandidateDiagnosticResult{}, err
	}
	value, ok := result.Result.(CandidateDiagnosticResult)
	if !ok {
		return CandidateDiagnosticResult{}, errors.New("invalid diagnostic result")
	}
	return value, nil
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
	result := CandidateDiagnosticResult{Lines: []CandidateDiagnosticLine{}, Observations: []CandidateObservation{}}
	if len(s.payload.Containers) == 0 {
		result.Unavailable = true
		return engine.ApplyResult{Result: result}, nil
	}
	if rt.Secrets == nil {
		result.Unavailable = true
		return engine.ApplyResult{Result: result}, nil
	}
	logsSafe := !s.payload.MetadataOnly
	if !logsSafe {
		result.Unavailable = true
	}
	// Docker's tail can begin inside a multiline secret. The missing prefix
	// makes exact redaction impossible, even without inserted timestamps.
	if rt.Secrets.RequiresWholeTextRedaction() {
		logsSafe = false
		result.Unavailable = true
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
		format := "{{json .Id}}\n{{json .Name}}\n{{json .Image}}\n{{json .Config.Labels}}\n{{json .State}}"
		inspect := exec.CommandContext(ctx, "docker", "inspect", "--format", format, c.Name)
		metadata := &limitedDiagnosticBuffer{limit: 64 * 1024}
		inspect.Stdout = metadata
		if err := inspect.Run(); err != nil || metadata.overflow {
			result.Unavailable = true
			continue
		}
		count := 5
		parts := strings.SplitN(metadata.String(), "\n", count)
		var id, name, imageID string
		var labels map[string]string
		var state struct {
			Status     string
			Running    bool
			StartedAt  time.Time
			FinishedAt time.Time
			ExitCode   int
			OOMKilled  bool
			Health     *struct{ Status string }
		}
		if len(parts) != count || json.Unmarshal([]byte(parts[0]), &id) != nil || json.Unmarshal([]byte(parts[1]), &name) != nil || json.Unmarshal([]byte(parts[2]), &imageID) != nil || json.Unmarshal([]byte(parts[3]), &labels) != nil || json.Unmarshal([]byte(parts[4]), &state) != nil || strings.TrimPrefix(name, "/") != c.Name || id == "" {
			result.Unavailable = true
			continue
		}
		if s.expectedImageID != "" {
			if imageID != s.expectedImageID {
				result.Unavailable = true
				result.ImageMismatch = true
				continue
			}
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
		observation := CandidateObservation{Process: c.Process, Replica: c.Replica, Container: c.Name, ContainerID: id, ObservedAt: time.Now().UTC(), State: state.Status, Running: state.Running, StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, OOMKilled: state.OOMKilled}
		if !state.StartedAt.IsZero() && !state.FinishedAt.IsZero() {
			observation.ExitCode = &state.ExitCode
		}
		if state.Health != nil {
			observation.Health = state.Health.Status
		}
		result.Observations = append(result.Observations, observation)
		if !logsSafe {
			continue
		}
		// Request one extra line so a bounded tail can be distinguished from
		// a complete stream of exactly 200 lines.
		// Do not ask Docker to insert timestamps into the stream: that would
		// split multiline secret values before they can be redacted.
		command := exec.CommandContext(ctx, "docker", "logs", "--tail", "201", id)
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
		text = rt.Secrets.Redact(output.SafeText(strings.ReplaceAll(text, "\r\n", "\n")))
		observedAt := time.Now().UTC()
		result.LogTimestampSource = "observation_time"
		containerLines := 0
		for _, line := range strings.Split(text, "\n") {
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
			message := line
			if len(message) > 32*1024 {
				message = diagnosticPrefix(message, 32*1024)
				result.Truncated = true
			}
			if len(message) > budget {
				message = diagnosticPrefix(message, budget)
				result.Truncated = true
			}
			result.Lines = append(result.Lines, CandidateDiagnosticLine{Process: c.Process, Replica: c.Replica, Container: c.Name, ContainerID: id, Timestamp: observedAt, Message: message})
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
