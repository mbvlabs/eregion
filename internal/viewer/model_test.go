package viewer

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/contract"
)

var testID = contract.OperationID("op_" + strings.Repeat("a", 32))

func op(state contract.State, attempt uint32, last uint64, created, updated time.Time) contract.Operation {
	return contract.Operation{
		ID:           testID,
		Kind:         "diagnostic.test",
		Target:       "control-plane",
		Actor:        contract.Actor{Kind: "ci", Subject: "test"},
		State:        state,
		Attempt:      attempt,
		LastSequence: last,
		CreatedAt:    created,
		UpdatedAt:    updated,
	}
}

func event(sequence uint64, at time.Time, kind string) contract.Event {
	return contract.Event{OperationID: testID, Sequence: sequence, At: at, Kind: kind, Attempt: 1}
}

func stateEvent(sequence uint64, at time.Time, state contract.State) contract.Event {
	value := event(sequence, at, "state")
	value.State = state
	return value
}

func stepEvent(sequence uint64, at time.Time, stage string) contract.Event {
	value := event(sequence, at, "step")
	value.Stage = stage
	return value
}

func logEvent(sequence uint64, at time.Time, message string) contract.Event {
	value := event(sequence, at, "log")
	value.Message = message
	return value
}

func baseTime() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }

func TestReducerOrderedStagesProgressAndSuccess(t *testing.T) {
	start := baseTime()
	model := New(testID, Options{})
	model.Snapshot(op(contract.Running, 1, 8, start, start.Add(8*time.Second)), start.Add(8*time.Second))
	progress := event(4, start.Add(4*time.Second), "progress")
	progress.Progress = &contract.Progress{Current: 2, Total: 3, Unit: "steps"}
	err := model.Page([]contract.Event{
		stateEvent(1, start, contract.Queued),
		stateEvent(2, start.Add(time.Second), contract.Running),
		stepEvent(3, start.Add(2*time.Second), "preflight"),
		progress,
		stepEvent(5, start.Add(5*time.Second), "build"),
		logEvent(6, start.Add(6*time.Second), "building"),
		stepEvent(7, start.Add(7*time.Second), "deploy"),
		stateEvent(8, start.Add(8*time.Second), contract.Succeeded),
	}, start.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if model.Cursor != 8 || model.EventCount != 8 || len(model.EventLines) != 8 {
		t.Fatalf("cursor=%d count=%d lines=%d", model.Cursor, model.EventCount, len(model.EventLines))
	}
	if !model.Terminal() || model.State != contract.Succeeded {
		t.Fatalf("state=%s", model.State)
	}
	if len(model.Stages) != 3 {
		t.Fatalf("stages=%+v", model.Stages)
	}
	for _, stage := range model.Stages {
		if stage.Status != StageCompleted {
			t.Fatalf("stage not completed: %+v", stage)
		}
	}
	if model.Progress == nil || model.Progress.Current != 2 || model.Progress.Total != 3 {
		t.Fatalf("progress=%+v", model.Progress)
	}
	if model.ObservationState != Live {
		t.Fatalf("observation=%s", model.ObservationState)
	}
	elapsed, ok := model.Elapsed(start.Add(9 * time.Second))
	if !ok || elapsed != 8*time.Second {
		t.Fatalf("terminal elapsed=%s ok=%v", elapsed, ok)
	}
	queued, ok := model.Queued(start.Add(9 * time.Second))
	if !ok || queued != time.Second {
		t.Fatalf("queued=%s ok=%v", queued, ok)
	}
}

// A viewer that attaches after completion sees the terminal snapshot before it
// replays the step events. The snapshot is authoritative, so the replayed stage
// must not remain "running" forever.
func TestTerminalSnapshotFinishesReplayedStages(t *testing.T) {
	start := baseTime()
	model := New(testID, Options{})
	model.Snapshot(op(contract.Succeeded, 1, 3, start, start.Add(3*time.Second)), start.Add(3*time.Second))
	err := model.Page([]contract.Event{
		stateEvent(1, start, contract.Queued),
		stepEvent(2, start.Add(time.Second), "preflight"),
		stateEvent(3, start.Add(2*time.Second), contract.Succeeded),
	}, start.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Stages) != 1 || model.Stages[0].Status != StageCompleted {
		t.Fatalf("replayed stage after a terminal snapshot is not complete: %+v", model.Stages)
	}
	if _, running := model.ActiveStage(); running {
		t.Fatal("a completed operation still advertises a running stage")
	}
}

func TestReducerDropsReplayedAndDuplicateEvents(t *testing.T) {
	at := baseTime()
	model := New(testID, Options{})
	first := []contract.Event{stateEvent(1, at, contract.Queued), logEvent(2, at, "one")}
	if err := model.Page(first, at); err != nil {
		t.Fatal(err)
	}
	if err := model.Page(first, at); err != nil {
		t.Fatal(err)
	}
	if err := model.Page([]contract.Event{logEvent(2, at, "one"), logEvent(3, at, "two")}, at); err != nil {
		t.Fatal(err)
	}
	if model.EventCount != 3 || model.Cursor != 3 {
		t.Fatalf("count=%d cursor=%d", model.EventCount, model.Cursor)
	}
	if len(model.EventLines) != 3 || model.EventLines[2].Message != "two" {
		t.Fatalf("events=%+v", model.EventLines)
	}
}

func TestReducerReportsGapWithoutAdvancing(t *testing.T) {
	at := baseTime()
	model := New(testID, Options{})
	if err := model.Page([]contract.Event{logEvent(1, at, "one")}, at); err != nil {
		t.Fatal(err)
	}
	err := model.Page([]contract.Event{logEvent(3, at, "three")}, at)
	var gap *GapError
	if !errors.As(err, &gap) || gap.Expected != 2 || gap.Actual != 3 {
		t.Fatalf("gap=%v", err)
	}
	if model.Cursor != 1 || !model.Gap || model.ExpectedSequence != 2 {
		t.Fatalf("cursor=%d gap=%v expected=%d", model.Cursor, model.Gap, model.ExpectedSequence)
	}
	if ErrGap(err) == nil || ErrGap(err).Code != contract.PreconditionFailed {
		t.Fatal("gap must be a precondition_failed fault")
	}
}

func TestReducerSnapshotAuthorityAndAttemptChange(t *testing.T) {
	start := baseTime()
	model := New(testID, Options{})
	if err := model.Page([]contract.Event{
		stateEvent(1, start, contract.Queued),
		stateEvent(2, start.Add(time.Second), contract.Running),
	}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	model.Snapshot(op(contract.Running, 2, 5, start, start.Add(3*time.Second)), start.Add(3*time.Second))
	if model.Attempt != 2 || !model.AttemptChanged || model.PreviousAttempt != 1 {
		t.Fatalf("attempt=%d previous=%d changed=%v", model.Attempt, model.PreviousAttempt, model.AttemptChanged)
	}
	model.Snapshot(op(contract.Succeeded, 2, 6, start, start.Add(4*time.Second)), start.Add(4*time.Second))
	if !model.Terminal() || model.State != contract.Succeeded {
		t.Fatalf("state=%s", model.State)
	}
	// This snapshot is older than the accepted sequence and must not rewrite state.
	model.Snapshot(op(contract.Running, 2, 5, start, start.Add(3*time.Second)), start.Add(3*time.Second))
	if model.State != contract.Succeeded {
		t.Fatalf("stale snapshot rewrote state: %s", model.State)
	}
	if model.Cursor != 2 {
		t.Fatalf("snapshot changed event cursor: %d", model.Cursor)
	}
}

func TestReducerStaleReconnectAndCancellation(t *testing.T) {
	at := baseTime()
	model := New(testID, Options{})
	model.Snapshot(op(contract.Running, 1, 1, at, at), at)
	if model.ObservationState != Live {
		t.Fatalf("observation=%s", model.ObservationState)
	}
	model.PollFailed(errors.New("network lost"), at.Add(time.Second))
	if model.ObservationState != Reconnecting {
		t.Fatalf("first failure=%s", model.ObservationState)
	}
	model.PollFailed(errors.New("network lost"), at.Add(2*time.Second))
	model.PollFailed(errors.New("network lost"), at.Add(3*time.Second))
	if model.ObservationState != Stale {
		t.Fatalf("third failure=%s", model.ObservationState)
	}
	model.PollSucceeded(at.Add(4 * time.Second))
	if model.ObservationState != Live || model.Failures != 0 {
		t.Fatalf("recovery=%s failures=%d", model.ObservationState, model.Failures)
	}
	model.Tick(at.Add(15 * time.Second))
	if model.ObservationState != Stale {
		t.Fatalf("tick did not stale: %s", model.ObservationState)
	}
	model.Snapshot(op(contract.Cancelling, 1, 2, at, at.Add(16*time.Second)), at.Add(16*time.Second))
	if !model.CancelRequested || model.CancelAcknowledged {
		t.Fatalf("cancelling flags=%v/%v", model.CancelRequested, model.CancelAcknowledged)
	}
	model.Snapshot(op(contract.Canceled, 1, 3, at, at.Add(17*time.Second)), at.Add(17*time.Second))
	if !model.CancelRequested || !model.CancelAcknowledged {
		t.Fatalf("cancelled flags=%v/%v", model.CancelRequested, model.CancelAcknowledged)
	}
}

func TestReducerFailureCodeAndStages(t *testing.T) {
	at := baseTime()
	model := New(testID, Options{})
	if err := model.Page([]contract.Event{
		stepEvent(1, at, "preflight"),
		stepEvent(2, at.Add(time.Second), "deploy"),
	}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	failure := stateEvent(3, at.Add(2*time.Second), contract.Failed)
	failure.ErrorCode = contract.Unavailable
	if err := model.Page([]contract.Event{failure}, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if model.ErrorCode != contract.Unavailable || model.State != contract.Failed {
		t.Fatalf("state=%s code=%s", model.State, model.ErrorCode)
	}
	if model.Stages[0].Status != StageCompleted || model.Stages[1].Status != StageFailed {
		t.Fatalf("stages=%+v", model.Stages)
	}
}

func TestReducerBoundsEventRing(t *testing.T) {
	at := baseTime()
	model := New(testID, Options{EventLimit: 2})
	for sequence := uint64(1); sequence <= 5; sequence++ {
		if err := model.Page([]contract.Event{logEvent(sequence, at.Add(time.Duration(sequence)*time.Second), "line")}, at); err != nil {
			t.Fatal(err)
		}
	}
	if len(model.EventLines) != 2 || model.EventLines[0].Sequence != 4 || model.EventLines[1].Sequence != 5 {
		t.Fatalf("ring=%+v", model.EventLines)
	}
	if model.EventCount != 5 {
		t.Fatalf("count=%d", model.EventCount)
	}
}

func TestReducerSanitizesAndRedactsEventText(t *testing.T) {
	at := baseTime()
	clean := func(text string) string {
		return DefaultClean(strings.ReplaceAll(text, "hunter2", "[REDACTED]"))
	}
	model := New(testID, Options{Clean: clean})
	hostile := "\x1b]52;c;clipboard\a\rsecret hunter2"
	if err := model.Page([]contract.Event{logEvent(1, at, hostile)}, at); err != nil {
		t.Fatal(err)
	}
	message := model.EventLines[0].Message
	if strings.ContainsAny(message, "\x1b\a\r") || strings.Contains(message, "hunter2") {
		t.Fatalf("unsafe message: %q", message)
	}
	if !strings.Contains(message, `\u001b`) {
		t.Fatalf("control character was not made visible: %q", message)
	}
}

// Wide labels must survive sanitization intact; truncation belongs to the
// renderer, which measures display width rather than bytes.
func TestWideLabelsAndEmojiArePreservedAndSafe(t *testing.T) {
	at := baseTime()
	model := New(testID, Options{})
	step := stepEvent(1, at, "部署-数据库 ✅")
	step.Message = "起動中 🚀"
	log := logEvent(2, at.Add(time.Second), "文字幅のテスト 🧪")
	if err := model.Page([]contract.Event{step, log}, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(model.Stages) != 1 || model.Stages[0].Name != "部署-数据库 ✅" {
		t.Fatalf("stages = %+v", model.Stages)
	}
	if model.EventLines[0].Message != "起動中 🚀" || model.EventLines[1].Message != "文字幅のテスト 🧪" {
		t.Fatalf("event text = %+v", model.EventLines)
	}
	for _, text := range []string{model.Stages[0].Name, model.EventLines[0].Message, model.EventLines[1].Message} {
		if !utf8.ValidString(text) || strings.ContainsRune(text, '\x1b') {
			t.Fatalf("unsafe wide text: %q", text)
		}
	}
}
