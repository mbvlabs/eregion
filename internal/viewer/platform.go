package viewer

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// TargetRole names what a platform step touches. The names come from the step
// IDs the platform worker actually emits; they are not a client-side plan.
type TargetRole string

const (
	TargetBuild           TargetRole = "build"
	TargetCandidate       TargetRole = "candidate"
	TargetCandidateHealth TargetRole = "candidate health"
	TargetTrafficSwitch   TargetRole = "traffic switch"
	TargetServingHealth   TargetRole = "serving health"
	TargetRollback        TargetRole = "rollback"
	TargetRestoreHealth   TargetRole = "rollback health"
	TargetCleanup         TargetRole = "cleanup"
	TargetNode            TargetRole = "node"
	TargetStep            TargetRole = "step"
)

func (r TargetRole) String() string { return string(r) }

// DeploymentTarget is one observed target with the state its step events
// proved. It is derived from stages, so a target is never shown as pending
// before any event mentions it.
type DeploymentTarget struct {
	Role     TargetRole
	Label    string
	Stage    string
	Status   StageStatus
	Attempt  uint32
	LastSeen time.Time
}

// BuildLine is one log observation attributed to a build-capable stage. The
// platform worker stamps the durable step on log events; older events without a
// step are attributed to the most recently observed step.
type BuildLine struct {
	Sequence uint64
	At       time.Time
	Stage    string
	Message  string
}

// HealthObservation is one explicit probe result. Structured fields come from
// the worker's health event; the message parser remains only as a fallback for
// events written before structured observations existed.
type HealthObservation struct {
	Sequence       uint64
	At             time.Time
	Stage          string
	Attempt        uint32
	Target         string
	TimeoutSeconds int
	Passed         bool
	Checks         int
	Detail         string
}

// ServingRelease is the release evidence present in event data. Structured
// events name the serving release; older streams only carry the container log
// line and a restore warning.
type ServingRelease struct {
	Observed  bool
	ReleaseID string
	Image     string
	Container string
	Health    string
	At        time.Time
	Sequence  uint64
	Source    string
}

// CleanupObservation states whether the candidate cleanup step was observed.
// Cleanup is a durable step, so its absence is a fact the viewer can report.
type CleanupObservation struct {
	Observed bool
	Status   StageStatus
	At       time.Time
	Sequence uint64
	Duration time.Duration
}

// healthPassedPattern is a compatibility fallback for streams that predate
// structured health events. New events always carry contract.Health.
var (
	healthPassedPattern  = regexp.MustCompile(`^health passed after ([0-9]+) check\(s\): (.*)$`)
	containerRunsPattern = regexp.MustCompile(`^container ([^ ]+) runs ([^ ]+)$`)
	releaseIDPattern     = regexp.MustCompile(`rel_[0-9a-f]{32}`)
)

// classifyStage maps an emitted step ID to a target role. Unknown stages stay a
// generic step: the viewer never guesses that a stage is a build or a deploy.
func classifyStage(stage string) (TargetRole, string) {
	lower := strings.ToLower(stage)
	switch {
	case strings.HasPrefix(lower, "platform-pack-"):
		return TargetBuild, "build"
	case strings.HasPrefix(lower, "platform-pull-"):
		return TargetBuild, "pull image"
	case strings.HasPrefix(lower, "platform-image-"):
		return TargetBuild, "resolve image"
	case strings.HasPrefix(lower, "platform-command-"):
		return TargetBuild, "release command"
	case strings.HasPrefix(lower, "platform-preflight-"):
		return TargetBuild, "preflight"
	case strings.HasPrefix(lower, "platform-container-01-"):
		return TargetCandidate, "candidate container"
	case strings.HasPrefix(lower, "platform-health-01-"):
		return TargetCandidateHealth, "candidate health"
	case strings.HasPrefix(lower, "platform-container-02-"):
		return TargetTrafficSwitch, "traffic switch"
	case strings.HasPrefix(lower, "platform-health-02-"):
		return TargetServingHealth, "serving health"
	case strings.HasPrefix(lower, "platform-container-03-"):
		return TargetRollback, "restore previous release"
	case strings.HasPrefix(lower, "platform-health-03-"):
		return TargetRestoreHealth, "restore health"
	case strings.HasPrefix(lower, "platform-cleanup-"):
		return TargetCleanup, "cleanup"
	case strings.HasPrefix(lower, "platform-node-"):
		return TargetNode, "node check"
	case strings.HasPrefix(lower, "platform-"):
		return TargetStep, stage
	case strings.Contains(lower, "pack"), strings.Contains(lower, "build"):
		return TargetBuild, stage
	default:
		return TargetStep, stage
	}
}

func (m *Model) observePlatformStep(event contract.Event) {
	role, label := classifyStage(event.Stage)
	m.currentStage = event.Stage
	m.currentRole = role
	m.currentLabel = label
	if role == TargetCleanup {
		m.Cleanup.Observed = true
		m.Cleanup.Status = StageRunning
		m.Cleanup.At = event.At
		m.Cleanup.Sequence = event.Sequence
	}
	m.syncTargets()
}

func (m *Model) syncTargets() {
	if len(m.Stages) == 0 {
		m.Targets = nil
		return
	}
	targets := make([]DeploymentTarget, 0, len(m.Stages))
	for _, stage := range m.Stages {
		role, label := classifyStage(stage.Name)
		targets = append(targets, DeploymentTarget{
			Role: role, Label: label, Stage: stage.Name,
			Status: stage.Status, Attempt: stage.Attempt, LastSeen: stage.LastSeen,
		})
	}
	m.Targets = targets
}

func (m *Model) observeLog(event contract.Event) {
	stage := strings.TrimSpace(event.Stage)
	if stage == "" {
		stage = strings.TrimSpace(event.Step)
	}
	if stage == "" {
		stage = m.currentStage
	}
	role, _ := classifyStage(stage)
	if role == TargetBuild {
		m.BuildLog = append(m.BuildLog, BuildLine{
			Sequence: event.Sequence, At: event.At, Stage: stage, Message: event.Message,
		})
		if limit := m.eventLimit; limit > 0 && len(m.BuildLog) > limit {
			m.BuildLog = m.BuildLog[len(m.BuildLog)-limit:]
		}
	}
	if event.Health != nil {
		m.observeStructuredHealth(event, stage)
	} else {
		m.observeHealthLog(event, stage, role)
	}
	m.observeServing(event, role)
	m.observeContainerLog(event, role)
	m.observeCleanup(event, role)
	m.observeStepDuration(event)
	m.observeReleaseFallback(event)
}

// observeStructuredHealth prefers the worker's explicit probe fields. If the
// node already emitted a failed health warning for the same stage and attempt,
// this replaces that message-derived record instead of duplicating it.
func (m *Model) observeStructuredHealth(event contract.Event, stage string) {
	health := event.Health
	attempt := event.Attempt
	if attempt == 0 {
		attempt = m.Attempt
	}
	observation := HealthObservation{
		Sequence: event.Sequence, At: event.At, Stage: stage, Attempt: attempt,
		Target: health.Target, TimeoutSeconds: health.TimeoutSeconds,
		Passed: health.Passed, Checks: health.Checks, Detail: health.Detail,
	}
	for i := len(m.Health) - 1; i >= 0; i-- {
		previous := m.Health[i]
		if previous.Stage == stage && previous.Attempt == attempt {
			m.Health[i] = observation
			return
		}
	}
	m.Health = append(m.Health, observation)
}

// observeHealthLog parses the old human-readable message. It is kept only so a
// stream written before structured events still shows health evidence.
func (m *Model) observeHealthLog(event contract.Event, stage string, role TargetRole) {
	message := strings.TrimSpace(event.Message)
	if match := healthPassedPattern.FindStringSubmatch(message); match != nil {
		checks := 0
		_, _ = fmt.Sscan(match[1], &checks)
		m.Health = append(m.Health, HealthObservation{
			Sequence: event.Sequence, At: event.At, Stage: stage, Attempt: event.Attempt,
			Passed: true, Checks: checks, Detail: match[2],
		})
		return
	}
	if event.Kind == "warning" && (role == TargetCandidateHealth || role == TargetServingHealth || role == TargetRestoreHealth) {
		detail := strings.TrimPrefix(message, "Node step failed: ")
		m.Health = append(m.Health, HealthObservation{
			Sequence: event.Sequence, At: event.At, Stage: stage, Attempt: event.Attempt,
			Passed: false, Detail: detail,
		})
	}
}

func (m *Model) observeServing(event contract.Event, role TargetRole) {
	release := event.ServingReleaseID
	if release == "" && (role == TargetTrafficSwitch || role == TargetRollback) {
		release = event.ReleaseID
	}
	if release == "" {
		return
	}
	health := m.Serving.Health
	if event.Health != nil {
		if event.Health.Passed {
			health = "healthy"
		} else {
			health = "unhealthy"
		}
	}
	m.Serving = ServingRelease{
		Observed: true, ReleaseID: release, Image: m.Serving.Image, Container: m.Serving.Container,
		Health: health, At: event.At, Sequence: event.Sequence, Source: string(role),
	}
}

func (m *Model) observeContainerLog(event contract.Event, role TargetRole) {
	if role != TargetTrafficSwitch && role != TargetRollback {
		return
	}
	match := containerRunsPattern.FindStringSubmatch(strings.TrimSpace(event.Message))
	if match == nil {
		return
	}
	releaseID := m.Serving.ReleaseID
	if releaseID == "" {
		releaseID = event.ReleaseID
	}
	m.Serving = ServingRelease{
		Observed: true, ReleaseID: releaseID, Image: match[2], Container: match[1],
		Health: m.Serving.Health,
		At:     event.At, Sequence: event.Sequence, Source: string(role),
	}
}

func (m *Model) observeCleanup(event contract.Event, role TargetRole) {
	if role != TargetCleanup {
		return
	}
	m.Cleanup.Observed = true
	m.Cleanup.At = event.At
	m.Cleanup.Sequence = event.Sequence
	if event.DurationMS != nil {
		m.Cleanup.Status = StageCompleted
		m.Cleanup.Duration = time.Duration(*event.DurationMS) * time.Millisecond
	}
	if event.Kind == "warning" {
		m.Cleanup.Status = StageFailed
	}
}

// observeStepDuration marks a stage complete from the worker's measured step
// completion event, so a finished stage does not stay "running" until the next
// step starts.
func (m *Model) observeStepDuration(event contract.Event) {
	if event.Step == "" || event.DurationMS == nil {
		return
	}
	for i := range m.Stages {
		if m.Stages[i].Name == event.Step {
			m.Stages[i].Status = StageCompleted
			m.Stages[i].LastSeen = event.At
			m.syncTargets()
			return
		}
	}
}

// observeReleaseFallback keeps old streams readable: a restore warning named the
// previous release before structured release fields existed.
func (m *Model) observeReleaseFallback(event contract.Event) {
	if m.Serving.ReleaseID != "" || event.ReleaseID != "" || event.ServingReleaseID != "" {
		return
	}
	if id := releaseIDPattern.FindString(event.Message); id != "" {
		m.Serving.ReleaseID = id
	}
}

// ServingLine is the explicit statement of which release is serving. It says
// what the events prove and names the missing event field when they prove
// nothing.
func (m *Model) ServingLine() string {
	if m.Serving.Observed {
		image := m.Serving.Image
		if image == "" {
			image = "image not reported"
		}
		release := m.Serving.ReleaseID
		if release == "" {
			release = "release ID not reported by events"
		}
		line := "serving release: " + release + " · image " + image
		if m.Serving.Health != "" {
			line += " · health " + m.Serving.Health
		}
		if m.Serving.Source == string(TargetRollback) {
			line += " (previous release restored)"
		}
		if m.Serving.Container != "" {
			line += " · container " + m.Serving.Container
		}
		return line
	}
	if m.Kind == "" {
		return "serving release: unknown · no observation yet"
	}
	if m.Kind != "platform.deploy" && m.Kind != "platform.rollback" {
		return "serving release: not applicable to " + m.Kind
	}
	return "serving release: unknown · no serving-container event was emitted"
}

// CleanupLine states whether the candidate cleanup step happened. The worker
// emits it as a durable step, so "not observed" is not the same as "pending".
func (m *Model) CleanupLine() string {
	if !m.Cleanup.Observed {
		return "candidate cleanup: not observed"
	}
	line := "candidate cleanup: " + string(m.Cleanup.Status)
	if m.Cleanup.Duration > 0 {
		line += " in " + FormatDuration(m.Cleanup.Duration)
	}
	return line
}

// ShortImage trims an image digest for a narrow header while keeping its
// identity copyable in the full log.
func ShortImage(image string) string {
	if !strings.HasPrefix(image, "sha256:") || len(image) <= 19 {
		return image
	}
	return image[:19] + "…"
}
