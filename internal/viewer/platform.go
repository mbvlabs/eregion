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
// platform worker does not stamp the step ID on log events; the reducer
// attributes a line only when the most recently observed step is a build step.
type BuildLine struct {
	Sequence uint64
	At       time.Time
	Stage    string
	Message  string
}

// HealthObservation is an explicit probe result observed in a log or warning
// event. Checks is zero when the event did not report a probe count.
type HealthObservation struct {
	Sequence uint64
	At       time.Time
	Stage    string
	Attempt  uint32
	Passed   bool
	Checks   int
	Detail   string
}

// ServingRelease is the release evidence present in event data. The worker
// emits the serving container's immutable image in a log line but not the
// release ID, so ReleaseID is often empty and the presentation must say so.
type ServingRelease struct {
	Observed  bool
	ReleaseID string
	Image     string
	Container string
	At        time.Time
	Sequence  uint64
	Source    string
}

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
	m.observeHealthLog(event, stage, role)
	m.observeContainerLog(event, role)
	m.observeReleaseID(event)
}

// observeHealthLog turns the health step's explicit log/warning observation
// into a health record. The log event itself carries no stage, so it is
// attributed to the stage that was active when it arrived.
func (m *Model) observeHealthLog(event contract.Event, stage string, role TargetRole) {
	message := strings.TrimSpace(event.Message)
	if role == TargetCandidateHealth || role == TargetServingHealth || role == TargetRestoreHealth {
		if match := healthPassedPattern.FindStringSubmatch(message); match != nil {
			checks := 0
			_, _ = fmt.Sscan(match[1], &checks)
			m.Health = append(m.Health, HealthObservation{
				Sequence: event.Sequence, At: event.At, Stage: stage, Attempt: event.Attempt,
				Passed: true, Checks: checks, Detail: match[2],
			})
			return
		}
		if event.Kind == "warning" {
			detail := strings.TrimPrefix(message, "Node step failed: ")
			m.Health = append(m.Health, HealthObservation{
				Sequence: event.Sequence, At: event.At, Stage: stage, Attempt: event.Attempt,
				Passed: false, Detail: detail,
			})
		}
	}
	// A health success can be replayed without its step event when the viewer
	// joined late. Accept the unambiguous message, but never invent a stage.
	if match := healthPassedPattern.FindStringSubmatch(message); match != nil {
		if role != TargetCandidateHealth && role != TargetServingHealth && role != TargetRestoreHealth {
			checks := 0
			_, _ = fmt.Sscan(match[1], &checks)
			m.Health = append(m.Health, HealthObservation{
				Sequence: event.Sequence, At: event.At, Stage: stage, Attempt: event.Attempt,
				Passed: true, Checks: checks, Detail: match[2],
			})
		}
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
	m.Serving = ServingRelease{
		Observed: true, ReleaseID: releaseID, Image: match[2], Container: match[1],
		At: event.At, Sequence: event.Sequence, Source: string(role),
	}
}

// observeReleaseID picks up the only release identity the worker currently
// puts in a message: the previous release named when a failed verification
// restores the old container.
func (m *Model) observeReleaseID(event contract.Event) {
	if !strings.Contains(event.Message, "release") || m.Serving.ReleaseID != "" {
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

// ShortImage trims an image digest for a narrow header while keeping its
// identity copyable in the full log.
func ShortImage(image string) string {
	if !strings.HasPrefix(image, "sha256:") || len(image) <= 19 {
		return image
	}
	return image[:19] + "…"
}
