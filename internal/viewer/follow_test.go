package viewer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

type fakeClient struct {
	mu         sync.Mutex
	gets       int
	eventCalls int
	get        func(call int) (json.RawMessage, error)
	events     func(call int, after uint64, limit int) (json.RawMessage, error)
}

func (f *fakeClient) Get(_ context.Context, _ contract.OperationID) (json.RawMessage, error) {
	f.mu.Lock()
	f.gets++
	call := f.gets
	f.mu.Unlock()
	return f.get(call)
}

func (f *fakeClient) Events(_ context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	f.mu.Lock()
	f.eventCalls++
	call := f.eventCalls
	f.mu.Unlock()
	return f.events(call, cursor.After, limit)
}

func rawOperation(t *testing.T, value contract.Operation) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rawPage(t *testing.T, id contract.OperationID, after uint64, events []contract.Event) json.RawMessage {
	t.Helper()
	page := eventPage{Events: events, Cursor: contract.Cursor{OperationID: id, After: after}}
	if len(events) > 0 {
		page.Cursor.After = events[len(events)-1].Sequence
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFollowWaitsForCursorThenReturnsTerminal(t *testing.T) {
	at := baseTime()
	client := &fakeClient{
		get: func(int) (json.RawMessage, error) {
			return rawOperation(t, op(contract.Succeeded, 1, 3, at, at.Add(3*time.Second))), nil
		},
		events: func(_ int, after uint64, _ int) (json.RawMessage, error) {
			if after == 0 {
				return rawPage(t, testID, 0, []contract.Event{
					stateEvent(1, at, contract.Queued),
					stateEvent(2, at.Add(time.Second), contract.Running),
				}), nil
			}
			return rawPage(t, testID, after, []contract.Event{
				stateEvent(3, at.Add(3*time.Second), contract.Succeeded),
			}), nil
		},
	}
	var observations []Observation
	summary, err := Follow(context.Background(), client, testID, FollowOptions{PollInterval: time.Millisecond, PageLimit: 2}, func(observation Observation) error {
		observations = append(observations, observation)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != contract.Succeeded || summary.Cursor != 3 || summary.Events != 3 {
		t.Fatalf("summary=%+v", summary)
	}
	if client.eventCalls != 2 {
		t.Fatalf("event calls=%d; follow stopped before the snapshot cursor caught up", client.eventCalls)
	}
	if got := observationKinds(observations); !containsAll(got, []ObservationKind{ObservationSnapshot, ObservationEvents, ObservationClosed}) {
		t.Fatalf("observations=%v", got)
	}
}

func TestFollowRecoversFromConnectionLoss(t *testing.T) {
	at := baseTime()
	client := &fakeClient{
		get: func(call int) (json.RawMessage, error) {
			if call == 1 {
				return nil, contract.NewFault(contract.Unavailable, "network unavailable", "retry")
			}
			return rawOperation(t, op(contract.Succeeded, 1, 1, at, at.Add(time.Second))), nil
		},
		events: func(_ int, after uint64, _ int) (json.RawMessage, error) {
			return rawPage(t, testID, after, []contract.Event{stateEvent(1, at.Add(time.Second), contract.Succeeded)}), nil
		},
	}
	var sawFailed, sawLive bool
	summary, err := Follow(context.Background(), client, testID, FollowOptions{PollInterval: time.Millisecond}, func(observation Observation) error {
		sawFailed = sawFailed || observation.Kind == ObservationFailed
		sawLive = sawLive || observation.Observation == Live
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawFailed || !sawLive || summary.State != contract.Succeeded || client.gets < 2 {
		t.Fatalf("failed=%v live=%v summary=%+v gets=%d", sawFailed, sawLive, summary, client.gets)
	}
}

func TestFollowDetachDoesNotCancel(t *testing.T) {
	at := baseTime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &fakeClient{
		get: func(int) (json.RawMessage, error) {
			return rawOperation(t, op(contract.Running, 1, 1, at, at.Add(time.Second))), nil
		},
		events: func(_ int, after uint64, _ int) (json.RawMessage, error) {
			return rawPage(t, testID, after, []contract.Event{stateEvent(1, at.Add(time.Second), contract.Running)}), nil
		},
	}
	summary, err := Follow(ctx, client, testID, FollowOptions{PollInterval: time.Millisecond}, func(observation Observation) error {
		if observation.Kind == ObservationEvents {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if !summary.Detached || summary.State != contract.Running {
		t.Fatalf("summary=%+v", summary)
	}
	if contract.ExitCode(err) != 130 {
		t.Fatalf("exit=%d", contract.ExitCode(err))
	}
}

func TestFollowExpiredCursorIsFatalAndShown(t *testing.T) {
	at := baseTime()
	client := &fakeClient{
		get: func(int) (json.RawMessage, error) {
			return rawOperation(t, op(contract.Running, 1, 5, at, at.Add(time.Second))), nil
		},
		events: func(int, uint64, int) (json.RawMessage, error) {
			return nil, contract.NewFault(contract.PreconditionFailed, "event cursor expired", "The viewer cannot replay the missing history.")
		},
	}
	var failure Observation
	summary, err := Follow(context.Background(), client, testID, FollowOptions{PollInterval: time.Millisecond}, func(observation Observation) error {
		if observation.Kind == ObservationFailed {
			failure = observation
		}
		return nil
	})
	if contract.ExitCode(err) != 5 {
		t.Fatalf("exit=%d err=%v", contract.ExitCode(err), err)
	}
	if failure.Err == nil || summary.Cursor != 0 {
		t.Fatalf("failure=%+v summary=%+v", failure, summary)
	}
}

func TestFollowPersistentGapFails(t *testing.T) {
	at := baseTime()
	client := &fakeClient{
		get: func(int) (json.RawMessage, error) {
			return rawOperation(t, op(contract.Running, 1, 3, at, at.Add(time.Second))), nil
		},
		events: func(_ int, after uint64, _ int) (json.RawMessage, error) {
			if after == 0 {
				return rawPage(t, testID, 0, []contract.Event{logEvent(1, at, "one"), logEvent(3, at, "three")}), nil
			}
			return rawPage(t, testID, after, []contract.Event{logEvent(3, at, "three")}), nil
		},
	}
	summary, err := Follow(context.Background(), client, testID, FollowOptions{PollInterval: time.Millisecond}, nil)
	if contract.ExitCode(err) != 5 || summary.Cursor != 1 {
		t.Fatalf("exit=%d summary=%+v err=%v", contract.ExitCode(err), summary, err)
	}
}

func observationKinds(observations []Observation) []ObservationKind {
	kinds := make([]ObservationKind, 0, len(observations))
	for _, observation := range observations {
		kinds = append(kinds, observation.Kind)
	}
	return kinds
}

func containsAll(values []ObservationKind, wanted []ObservationKind) bool {
	set := map[ObservationKind]bool{}
	for _, value := range values {
		set[value] = true
	}
	for _, value := range wanted {
		if !set[value] {
			return false
		}
	}
	return true
}

func TestFollowSummaryOperationIdentity(t *testing.T) {
	at := baseTime()
	client := &fakeClient{
		get: func(int) (json.RawMessage, error) {
			return rawOperation(t, op(contract.Succeeded, 1, 1, at, at.Add(time.Second))), nil
		},
		events: func(_ int, after uint64, _ int) (json.RawMessage, error) {
			return rawPage(t, testID, after, []contract.Event{stateEvent(1, at.Add(time.Second), contract.Succeeded)}), nil
		},
	}
	summary, err := Follow(context.Background(), client, testID, FollowOptions{PollInterval: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Operation.ID != testID || summary.Operation.State != contract.Succeeded || strings.TrimSpace(summary.Operation.Kind) == "" {
		t.Fatalf("operation=%+v", summary.Operation)
	}
}
