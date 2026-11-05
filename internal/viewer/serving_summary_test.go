package viewer

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// TestFollowSummaryServingReleaseFollowsDeployOutcome covers the serving release
// named by the final follow summary. A failed first deploy must not inherit the
// candidate from the intermediate traffic-switch event, while a successful
// deploy or a failed redeploy keeps naming the release the deploy outcome says
// serves.
func TestFollowSummaryServingReleaseFollowsDeployOutcome(t *testing.T) {
	id := contract.OperationID("op_" + strings.Repeat("b", 32))
	start := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	previous := "rel_" + strings.Repeat("1", 32)
	candidate := "rel_" + strings.Repeat("2", 32)
	at := func(sequence uint64) time.Time {
		return start.Add(time.Duration(sequence) * time.Second)
	}
	routeSwitch := func(sequence uint64, release string) contract.Event {
		event := platformEvent(sequence, at(sequence), "log", "platform-routes-01-v1",
			"Caddy now load balances release "+release+" across 1 web replica(s).")
		event.ReleaseID = release
		event.ServingReleaseID = release
		return event
	}
	deploySucceeded := func(sequence uint64, release string) contract.Event {
		event := platformEvent(sequence, at(sequence), "log", "deploy",
			"Deployment succeeded; release "+release+" serves api/production.")
		event.ReleaseID = release
		event.ServingReleaseID = release
		return event
	}
	deployFailed := func(sequence uint64, release, serving string) contract.Event {
		message := "Deployment failed; failed release " + release + " is not recorded as serving"
		if serving != "" {
			message += "; release " + serving + " still serves"
		}
		event := platformEvent(sequence, at(sequence), "log", "deploy", message+".")
		event.ReleaseID = release
		event.ServingReleaseID = serving
		return event
	}
	terminal := func(sequence uint64, state contract.State, code contract.Code, message string) contract.Event {
		event := platformEvent(sequence, at(sequence), "state", "", message)
		event.State = state
		event.ErrorCode = code
		return event
	}

	tests := []struct {
		name        string
		events      []contract.Event
		wantServing string
	}{
		{
			name: "successful deploy",
			events: []contract.Event{
				platformEvent(1, at(1), "step", "platform-container-02-v1", "Node step started."),
				routeSwitch(2, candidate),
				platformEvent(3, at(3), "step", "platform-public-01-v1", "Node step started."),
				platformEvent(4, at(4), "log", "platform-public-01-v1", "Public route returned http status 200."),
				deploySucceeded(5, candidate),
				terminal(6, contract.Succeeded, "", "Platform action succeeded; domain state and history are durable."),
			},
			wantServing: candidate,
		},
		{
			name: "failed redeploy with previous serving release",
			events: []contract.Event{
				platformEvent(1, at(1), "step", "platform-container-02-v1", "Node step started."),
				routeSwitch(2, candidate),
				platformEvent(3, at(3), "step", "platform-public-01-v1", "Node step started."),
				platformEvent(4, at(4), "warning", "platform-public-01-v1", "Node step failed: public check failed"),
				platformEvent(5, at(5), "step", "platform-container-03-v1", "Node step started."),
				deployFailed(6, candidate, previous),
				terminal(7, contract.Failed, contract.PreconditionFailed, "the public route did not return a successful response"),
			},
			wantServing: previous,
		},
		{
			name: "failed first deploy before switch",
			events: []contract.Event{
				platformEvent(1, at(1), "step", "platform-health-01-v1", "Node step started."),
				platformEvent(2, at(2), "warning", "platform-health-01-v1", "Node step failed: health check did not pass"),
				deployFailed(3, candidate, ""),
				terminal(4, contract.Failed, contract.PreconditionFailed, "the candidate health check did not pass"),
			},
		},
		{
			name: "failed first deploy after switch",
			events: []contract.Event{
				platformEvent(1, at(1), "step", "platform-container-02-v1", "Node step started."),
				routeSwitch(2, candidate),
				platformEvent(3, at(3), "step", "platform-public-01-v1", "Node step started."),
				platformEvent(4, at(4), "warning", "platform-public-01-v1", "Node step failed: public check failed"),
				platformEvent(5, at(5), "step", "platform-container-03-v1", "Node step started."),
				deployFailed(6, candidate, ""),
				terminal(7, contract.Failed, contract.PreconditionFailed, "the public route did not return a successful response"),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := New(id, Options{})
			model.Snapshot(contract.Operation{
				ID: id, Kind: "platform.deploy", Target: "env/api/production",
				Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
				Attempt: 1, LastSequence: uint64(len(test.events)), CreatedAt: start, UpdatedAt: start,
			}, start)
			if err := model.Page(test.events, at(uint64(len(test.events)))); err != nil {
				t.Fatal(err)
			}
			summary := modelSummary(model)
			if summary.Serving.ReleaseID != test.wantServing {
				t.Fatalf("summary serving release = %q, want %q", summary.Serving.ReleaseID, test.wantServing)
			}

			out := &bytes.Buffer{}
			plain := NewPlain(out, out, nil)
			if err := plain.Finish(summary, nil, "", start.Add(5*time.Minute)); err != nil {
				t.Fatal(err)
			}
			line := out.String()
			if test.wantServing == "" {
				if strings.Contains(line, "serving release rel_") {
					t.Fatalf("failed deploy summary claims a serving release:\n%s", line)
				}
				return
			}
			if !strings.Contains(line, "serving release "+test.wantServing) {
				t.Fatalf("summary does not name serving release %q:\n%s", test.wantServing, line)
			}
		})
	}
}
