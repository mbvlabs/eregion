// Package viewer reduces operation observations into one presentation-neutral
// model. It deliberately has no Charm dependency: the follow loop, plain text,
// NDJSON, and the interactive terminal view all read the same reducer.
package viewer

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
)

// ObservationState describes what the viewer knows about its own connection.
// It is never confused with a domain operation state.
type ObservationState string

const (
	Connecting   ObservationState = "connecting"
	Live         ObservationState = "live"
	Stale        ObservationState = "stale"
	Reconnecting ObservationState = "reconnecting"
	Closed       ObservationState = "closed"
)

// StageStatus is derived only from observed events. There is no planned-stage
// list, so a stage is never invented as pending.
type StageStatus string

const (
	StageRunning   StageStatus = "running"
	StageCompleted StageStatus = "completed"
	StageFailed    StageStatus = "failed"
	StageStopped   StageStatus = "stopped"
)

// Stage is one execution stage observed in a step event.
type Stage struct {
	Name      string
	Status    StageStatus
	Attempt   uint32
	FirstSeen time.Time
	LastSeen  time.Time
}

// Cleaner makes untrusted server text safe for a terminal. Callers usually
// combine the invocation's redaction registry with output.SafeText.
type Cleaner func(string) string

func DefaultClean(text string) string { return output.SafeText(text) }

// Options bound the model and control staleness. Zero values select defaults.
type Options struct {
	EventLimit int
	StaleAfter time.Duration
	StalePolls int
	Clean      Cleaner
}

const (
	DefaultEventLimit = 2000
	DefaultStaleAfter = 10 * time.Second
	DefaultStalePolls = 3
)

func (o Options) normalized() Options {
	if o.EventLimit <= 0 {
		o.EventLimit = DefaultEventLimit
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	if o.StalePolls <= 0 {
		o.StalePolls = DefaultStalePolls
	}
	if o.Clean == nil {
		o.Clean = DefaultClean
	}
	return o
}

// Model is the reducer output. Its exported fields are current observations;
// private fields retain cursors and thresholds. A Model is owned by one
// goroutine at a time (the follow loop, or Bubble Tea's update loop).
type Model struct {
	OperationID contract.OperationID
	Kind        string
	Target      string
	Actor       contract.Actor

	State     contract.State
	Attempt   uint32
	ErrorCode contract.Code

	// PreviousAttempt and AttemptChanged are set when a newer attempt was
	// observed. The reducer never guesses why the attempt changed.
	PreviousAttempt uint32
	AttemptChanged  bool

	CreatedAt time.Time
	UpdatedAt time.Time

	// LastSequence is the snapshot's accepted-event sequence. Cursor is the
	// last event sequence this viewer applied. They are deliberately separate:
	// a snapshot may have been taken before the viewer fetched its events.
	LastSequence uint64
	Cursor       uint64
	EventCount   uint64
	EventLines   []contract.Event

	Stages   []Stage
	Progress *contract.Progress

	// Platform observations derived only from emitted events. Targets mirrors
	// the observed stages with their role; BuildLog, Health, and Serving hold
	// the explicit build, probe, and serving evidence.
	Targets  []DeploymentTarget
	BuildLog []BuildLine
	Health   []HealthObservation
	Serving  ServingRelease

	ObservationState ObservationState
	LastSuccessAt    time.Time
	LastError        string
	Failures         int
	StateChangedAt   time.Time
	FirstRunningAt   time.Time
	FirstEventAt     time.Time

	CancelRequested    bool
	CancelAcknowledged bool

	// Gap is true when the last page skipped a sequence. ExpectedSequence is
	// the first sequence the reducer needs before it can accept more events.
	Gap              bool
	ExpectedSequence uint64

	eventLimit    int
	staleAfter    time.Duration
	stalePolls    int
	clean         Cleaner
	startedAt     time.Time
	haveState     bool
	stateSequence uint64
	currentStage  string
	currentRole   TargetRole
	currentLabel  string
}

func New(id contract.OperationID, opts Options) *Model {
	opts = opts.normalized()
	return &Model{
		OperationID:      id,
		ObservationState: Connecting,
		eventLimit:       opts.EventLimit,
		staleAfter:       opts.StaleAfter,
		stalePolls:       opts.StalePolls,
		clean:            opts.Clean,
		startedAt:        time.Now(),
	}
}

// GapError reports an event page that skipped sequences. The follow loop
// refetches from Cursor rather than inventing the missing observations.
type GapError struct {
	Expected uint64
	Actual   uint64
}

func (e *GapError) Error() string {
	return fmt.Sprintf("event sequence gap: expected %d, received %d", e.Expected, e.Actual)
}

// Snapshot applies an authoritative operation snapshot. A snapshot whose
// last_sequence is older than one already applied does not rewrite state.
func (m *Model) Snapshot(op contract.Operation, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	m.observeSuccess(at)
	if m.OperationID == "" {
		m.OperationID = op.ID
	}
	m.Kind = m.clean(op.Kind)
	m.Target = m.clean(op.Target)
	m.Actor.Kind = m.clean(op.Actor.Kind)
	m.Actor.Subject = m.clean(op.Actor.Subject)
	m.CreatedAt = op.CreatedAt
	m.UpdatedAt = op.UpdatedAt

	authoritative := !m.haveState || (op.LastSequence >= m.LastSequence && op.LastSequence >= m.stateSequence)
	if op.LastSequence > m.LastSequence {
		m.LastSequence = op.LastSequence
	}
	if !authoritative {
		return
	}
	m.haveState = true
	m.applyState(op.State, op.Attempt, op.ErrorCode, op.UpdatedAt, op.LastSequence, true)
}

// Page applies one page of events in sequence order. Duplicates are dropped.
// A skipped sequence records a Gap and returns *GapError without advancing the
// cursor, so a refetch can replay the missing observations.
func (m *Model) Page(events []contract.Event, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	m.observeSuccess(at)

	ordered := append([]contract.Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })
	// Validate every event before applying any so a malformed page cannot
	// partially mutate the reducer. Sequence gaps are handled below, after the
	// valid prefix has been applied so a refetch resumes from the real cursor.
	for _, event := range ordered {
		if m.OperationID != "" && event.OperationID != m.OperationID {
			return fmt.Errorf("event belongs to a different operation")
		}
		if event.Validate() != nil {
			return fmt.Errorf("invalid event page")
		}
	}
	for _, event := range ordered {
		if event.Sequence <= m.Cursor {
			continue // duplicate delivery
		}
		expected := m.Cursor + 1
		if event.Sequence != expected {
			m.Gap = true
			m.ExpectedSequence = expected
			return &GapError{Expected: expected, Actual: event.Sequence}
		}
		clean, err := m.cleanEvent(event)
		if err != nil {
			return err
		}
		m.appendEvent(clean, at)
		m.Cursor = clean.Sequence
		m.EventCount++
		if m.FirstEventAt.IsZero() {
			m.FirstEventAt = clean.At
		}
		m.applyEvent(clean)
	}
	m.Gap = false
	m.ExpectedSequence = 0
	// A late-joining viewer can receive a terminal snapshot before replaying
	// the step events that led to it. The snapshot remains authoritative, so
	// no stage may claim to still be running after the operation finished.
	if m.State.Terminal() {
		finishedAt := at
		if !m.UpdatedAt.IsZero() {
			finishedAt = m.UpdatedAt
		}
		m.finishStages(m.State, finishedAt)
	}
	return nil
}

func (m *Model) cleanEvent(event contract.Event) (contract.Event, error) {
	if err := event.Validate(); err != nil {
		return contract.Event{}, err
	}
	event.Message = m.clean(event.Message)
	event.Stage = m.clean(event.Stage)
	event.ExecutionID = m.clean(event.ExecutionID)
	if event.Progress != nil {
		progress := *event.Progress
		progress.Unit = m.clean(progress.Unit)
		event.Progress = &progress
	}
	return event, nil
}

func (m *Model) appendEvent(event contract.Event, at time.Time) {
	if m.eventLimit <= 0 {
		return
	}
	m.EventLines = append(m.EventLines, event)
	if len(m.EventLines) > m.eventLimit {
		copy(m.EventLines, m.EventLines[len(m.EventLines)-m.eventLimit:])
		m.EventLines = m.EventLines[:m.eventLimit]
	}
	_ = at
}

func (m *Model) applyEvent(event contract.Event) {
	m.applyAttempt(event.Attempt, event.At)
	switch event.Kind {
	case "state":
		m.applyState(event.State, event.Attempt, event.ErrorCode, event.At, event.Sequence, false)
		m.observeReleaseID(event)
	case "step":
		m.observeStep(event)
		m.observePlatformStep(event)
	case "warning":
		if event.Stage != "" {
			m.markStage(event.Stage, StageFailed, event.Attempt, event.At)
		}
		m.observeLog(event)
	case "progress":
		if event.Progress != nil {
			progress := *event.Progress
			m.Progress = &progress
		}
	case "log":
		// Diagnostics are retained in EventLines; build and health evidence is
		// extracted without inventing stages.
		m.observeLog(event)
	}
}

func (m *Model) applyAttempt(attempt uint32, at time.Time) {
	if attempt == 0 || attempt == m.Attempt || attempt < m.Attempt {
		return
	}
	if m.Attempt != 0 {
		m.PreviousAttempt = m.Attempt
		m.AttemptChanged = true
	}
	m.Attempt = attempt
	for i := range m.Stages {
		if m.Stages[i].Status == StageRunning {
			m.Stages[i].Status = StageStopped
			m.Stages[i].LastSeen = at
		}
	}
	m.syncTargets()
}

func (m *Model) applyState(state contract.State, attempt uint32, code contract.Code, at time.Time, sequence uint64, snapshot bool) {
	if !state.Valid() {
		return
	}
	if !snapshot && state == contract.Running && m.FirstRunningAt.IsZero() {
		if at.IsZero() {
			at = time.Now()
		}
		m.FirstRunningAt = at
	}
	if !snapshot && sequence < m.stateSequence {
		return // historical replay; the snapshot is authoritative
	}
	m.applyAttempt(attempt, at)
	if snapshot {
		if sequence > m.stateSequence {
			m.stateSequence = sequence
		}
	} else {
		m.stateSequence = sequence
	}
	if m.State != state {
		m.StateChangedAt = at
	}
	m.State = state
	m.haveState = true
	if state == contract.Failed {
		m.ErrorCode = code
	} else {
		m.ErrorCode = ""
	}
	switch state {
	case contract.Running:
		// FirstRunningAt was recorded above before authority filtering.
	case contract.Cancelling:
		m.CancelRequested = true
	case contract.Canceled:
		m.CancelRequested = true
		m.CancelAcknowledged = true
	}
	if state.Terminal() {
		m.finishStages(state, at)
	}
}

func (m *Model) observeStep(event contract.Event) {
	name := strings.TrimSpace(event.Stage)
	if name == "" {
		return
	}
	for i := range m.Stages {
		stage := &m.Stages[i]
		if stage.Name == name {
			stage.Status = StageRunning
			stage.Attempt = event.Attempt
			stage.LastSeen = event.At
			m.completeOtherRunning(name, event.At)
			m.syncTargets()
			return
		}
	}
	m.Stages = append(m.Stages, Stage{Name: name, Status: StageRunning, Attempt: event.Attempt, FirstSeen: event.At, LastSeen: event.At})
	m.completeOtherRunning(name, event.At)
	m.syncTargets()
}

// The diagnostic engine is sequential: a newly observed step means the
// previously active observation reached its boundary. When a transport later
// supports parallel stages, this rule must be replaced by an explicit event.
func (m *Model) completeOtherRunning(current string, at time.Time) {
	for i := range m.Stages {
		if m.Stages[i].Name != current && m.Stages[i].Status == StageRunning {
			m.Stages[i].Status = StageCompleted
			m.Stages[i].LastSeen = at
		}
	}
}

func (m *Model) markStage(name string, status StageStatus, attempt uint32, at time.Time) {
	for i := range m.Stages {
		if m.Stages[i].Name == name {
			m.Stages[i].Status = status
			if attempt != 0 {
				m.Stages[i].Attempt = attempt
			}
			m.Stages[i].LastSeen = at
			m.syncTargets()
			return
		}
	}
}

func (m *Model) finishStages(state contract.State, at time.Time) {
	for i := range m.Stages {
		if m.Stages[i].Status != StageRunning {
			continue
		}
		switch state {
		case contract.Succeeded:
			m.Stages[i].Status = StageCompleted
		case contract.Failed:
			m.Stages[i].Status = StageFailed
		case contract.Canceled:
			m.Stages[i].Status = StageStopped
		}
		m.Stages[i].LastSeen = at
	}
	m.syncTargets()
}

// PollSucceeded marks a successful observation and recovers a stale view.
func (m *Model) PollSucceeded(at time.Time) {
	m.observeSuccess(at)
}

func (m *Model) observeSuccess(at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	m.LastSuccessAt = at
	m.Failures = 0
	m.LastError = ""
	if m.ObservationState == Connecting || m.ObservationState == Reconnecting || m.ObservationState == Stale {
		m.ObservationState = Live
	}
}

// PollFailed records a transport failure. Three failures or ten seconds
// without a successful response makes the view stale, never "running".
func (m *Model) PollFailed(err error, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	if m.ObservationState == Closed {
		return
	}
	m.Failures++
	if err != nil {
		m.LastError = m.clean(err.Error())
	}
	if m.shouldBeStale(at) {
		m.ObservationState = Stale
	} else {
		m.ObservationState = Reconnecting
	}
}

// Tick updates elapsed/staleness only. It never invents an event or a state.
func (m *Model) Tick(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	if m.ObservationState == Closed {
		return
	}
	if m.ObservationState == Live {
		if m.age(now) >= m.staleAfter {
			m.ObservationState = Stale
		}
		return
	}
	if m.ObservationState == Connecting || m.ObservationState == Reconnecting || m.ObservationState == Stale {
		if m.shouldBeStale(now) {
			m.ObservationState = Stale
		}
	}
}

func (m *Model) shouldBeStale(now time.Time) bool {
	if m.Failures >= m.stalePolls {
		return true
	}
	return m.age(now) >= m.staleAfter
}

func (m *Model) age(now time.Time) time.Duration {
	since := m.LastSuccessAt
	if since.IsZero() {
		since = m.startedAt
	}
	age := now.Sub(since)
	if age < 0 {
		return 0
	}
	return age
}

// Close stops active observation. It is not an operation cancellation.
func (m *Model) Close(at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	m.ObservationState = Closed
	m.Failures = 0
	_ = at
}

func (m *Model) Terminal() bool { return m.State.Terminal() }

func (m *Model) Age(now time.Time) time.Duration { return m.age(now) }

// Freshness renders the observation age without claiming the operation is
// alive or stopped.
func (m *Model) Freshness(now time.Time) string {
	age := formatDuration(m.age(now))
	switch m.ObservationState {
	case Connecting:
		return "connecting · no observation yet"
	case Live:
		return "live · last observation " + age + " ago"
	case Reconnecting:
		return "reconnecting · last observation " + age + " ago"
	case Stale:
		last := string(m.State)
		if last == "" {
			last = "unknown"
		}
		return "stale · last known " + last + " " + age + " ago"
	case Closed:
		return "closed · observation complete"
	default:
		return string(m.ObservationState)
	}
}

// Elapsed is the server-timestamped lifetime of the operation. While it is
// nonterminal the client clock advances; at a terminal snapshot UpdatedAt is
// used so the final duration does not keep growing.
func (m *Model) Elapsed(now time.Time) (time.Duration, bool) {
	if m.CreatedAt.IsZero() {
		return 0, false
	}
	end := now
	if m.Terminal() && !m.UpdatedAt.IsZero() {
		end = m.UpdatedAt
	}
	if end.Before(m.CreatedAt) {
		return 0, true
	}
	return end.Sub(m.CreatedAt), true
}

// Queued reports time spent before the first observed running state. A queued
// operation reports its current queue wait; a late-joining viewer that never
// observed the transition reports unknown instead of guessing.
func (m *Model) Queued(now time.Time) (time.Duration, bool) {
	if m.CreatedAt.IsZero() {
		return 0, false
	}
	if m.State == contract.Queued {
		if now.Before(m.CreatedAt) {
			return 0, true
		}
		return now.Sub(m.CreatedAt), true
	}
	if m.FirstRunningAt.IsZero() || m.FirstRunningAt.Before(m.CreatedAt) {
		return 0, false
	}
	return m.FirstRunningAt.Sub(m.CreatedAt), true
}

// ActiveStage returns the stage currently observed running, if any.
func (m *Model) ActiveStage() (Stage, bool) {
	for i := len(m.Stages) - 1; i >= 0; i-- {
		if m.Stages[i].Status == StageRunning {
			return m.Stages[i], true
		}
	}
	return Stage{}, false
}

// StageCounts returns observed counts only. It is not an estimate of total work.
func (m *Model) StageCounts() (completed, active, failed int) {
	for _, stage := range m.Stages {
		switch stage.Status {
		case StageCompleted:
			completed++
		case StageRunning:
			active++
		case StageFailed:
			failed++
		}
	}
	return completed, active, failed
}

func (m *Model) Clone() *Model {
	clone := *m
	clone.EventLines = append([]contract.Event(nil), m.EventLines...)
	clone.Stages = append([]Stage(nil), m.Stages...)
	clone.Targets = append([]DeploymentTarget(nil), m.Targets...)
	clone.BuildLog = append([]BuildLine(nil), m.BuildLog...)
	clone.Health = append([]HealthObservation(nil), m.Health...)
	if m.Progress != nil {
		progress := *m.Progress
		clone.Progress = &progress
	}
	return &clone
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	minutes := int(d / time.Minute)
	seconds := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dm%02ds", minutes, seconds)
}

// FormatDuration is the shared human duration format used by presenters.
func FormatDuration(d time.Duration) string { return formatDuration(d) }

// ErrGap converts a reducer error into a machine-readable fault.
func ErrGap(err error) *contract.Fault {
	var gap *GapError
	if errors.As(err, &gap) {
		return contract.NewFault(contract.PreconditionFailed, "operation event history has a gap", "Refetch from the last event cursor; do not skip missing observations.")
	}
	return nil
}
