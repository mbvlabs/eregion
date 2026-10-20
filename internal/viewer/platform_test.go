package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

func platformEvent(sequence uint64, at time.Time, kind, stage, message string) contract.Event {
	return contract.Event{
		OperationID: "op_" + contract.OperationID(strings.Repeat("b", 32)),
		Sequence:    sequence, At: at, Kind: kind, Attempt: 1, Stage: stage, Message: message,
	}
}

func TestPlatformTargetsBuildHealthAndServing(t *testing.T) {
	id := contract.OperationID("op_" + strings.Repeat("b", 32))
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	model := New(id, Options{})
	model.Snapshot(contract.Operation{
		ID: id, Kind: "platform.deploy", Target: "env/api/production",
		Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
		Attempt: 1, LastSequence: 10, CreatedAt: start, UpdatedAt: start.Add(10 * time.Second),
	}, start.Add(10*time.Second))
	events := []contract.Event{
		platformEvent(1, start, "step", "platform-pack-01-v1", "Node step started."),
		platformEvent(2, start.Add(time.Second), "log", "", "stdout: building api"),
		platformEvent(3, start.Add(2*time.Second), "step", "platform-container-01-v1", "Node step started."),
		platformEvent(4, start.Add(3*time.Second), "log", "", "container eregion-env-candidate-1 runs sha256:"+strings.Repeat("a", 64)),
		platformEvent(5, start.Add(4*time.Second), "step", "platform-health-01-v1", "Node step started."),
		platformEvent(6, start.Add(5*time.Second), "log", "", "health passed after 3 check(s): http status 200"),
		platformEvent(7, start.Add(6*time.Second), "step", "platform-container-02-v1", "Node step started."),
		platformEvent(8, start.Add(7*time.Second), "log", "", "container eregion-env-serving runs sha256:"+strings.Repeat("c", 64)),
		platformEvent(9, start.Add(8*time.Second), "step", "platform-health-02-v1", "Node step started."),
		platformEvent(10, start.Add(9*time.Second), "log", "", "health passed after 2 check(s): http status 204"),
	}
	if err := model.Page(events, start.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(model.Targets) != 5 {
		t.Fatalf("targets = %+v", model.Targets)
	}
	roles := map[TargetRole]StageStatus{}
	for _, target := range model.Targets {
		roles[target.Role] = target.Status
	}
	for _, role := range []TargetRole{TargetBuild, TargetCandidate, TargetCandidateHealth, TargetTrafficSwitch, TargetServingHealth} {
		if _, ok := roles[role]; !ok {
			t.Fatalf("missing target role %q in %+v", role, model.Targets)
		}
	}
	if len(model.BuildLog) != 1 || !strings.Contains(model.BuildLog[0].Message, "building api") {
		t.Fatalf("build log = %+v", model.BuildLog)
	}
	if len(model.Health) != 2 || !model.Health[0].Passed || model.Health[0].Checks != 3 || model.Health[1].Checks != 2 {
		t.Fatalf("health = %+v", model.Health)
	}
	if !model.Serving.Observed || !strings.Contains(model.Serving.Image, "sha256:") || model.Serving.Container != "eregion-env-serving" {
		t.Fatalf("serving = %+v", model.Serving)
	}
	line := model.ServingLine()
	if !strings.Contains(line, "release ID not reported") || !strings.Contains(line, "container eregion-env-serving") {
		t.Fatalf("serving line = %q", line)
	}
}

func TestPlatformFailureRestoresPreviousReleaseAndHealth(t *testing.T) {
	id := contract.OperationID("op_" + strings.Repeat("b", 32))
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	model := New(id, Options{})
	model.Snapshot(contract.Operation{
		ID: id, Kind: "platform.deploy", Target: "env/api/production",
		Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
		Attempt: 1, LastSequence: 6, CreatedAt: start, UpdatedAt: start,
	}, start)
	events := []contract.Event{
		platformEvent(1, start, "step", "platform-container-02-v1", "Node step started."),
		platformEvent(2, start.Add(time.Second), "log", "", "container eregion-env-serving runs sha256:"+strings.Repeat("d", 64)),
		platformEvent(3, start.Add(2*time.Second), "step", "platform-health-02-v1", "Node step started."),
		platformEvent(4, start.Add(3*time.Second), "warning", "platform-health-02-v1", "Node step failed: health check did not pass within 30s; the previous release rel_00000000000000000000000000000002 was restored"),
		platformEvent(5, start.Add(4*time.Second), "step", "platform-container-03-v1", "Node step started."),
		platformEvent(6, start.Add(5*time.Second), "log", "", "container eregion-env-serving runs sha256:"+strings.Repeat("e", 64)),
	}
	if err := model.Page(events, start.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(model.Health) != 1 || model.Health[0].Passed || !strings.Contains(model.Health[0].Detail, "did not pass") {
		t.Fatalf("health = %+v", model.Health)
	}
	if model.Serving.Source != string(TargetRollback) || !strings.Contains(model.Serving.Image, strings.Repeat("e", 8)) {
		t.Fatalf("serving = %+v", model.Serving)
	}
	if model.Serving.ReleaseID != "rel_00000000000000000000000000000002" {
		t.Fatalf("release identity was lost: %+v", model.Serving)
	}
	if !strings.Contains(model.ServingLine(), "previous release restored") {
		t.Fatalf("serving line = %q", model.ServingLine())
	}
}

func TestStructuredEventsNameServingReleaseHealthAndCleanup(t *testing.T) {
	id := contract.OperationID("op_" + strings.Repeat("b", 32))
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	model := New(id, Options{})
	model.Snapshot(contract.Operation{
		ID: id, Kind: "platform.deploy", Target: "env/api/production",
		Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
		Attempt: 2, LastSequence: 8, CreatedAt: start, UpdatedAt: start,
	}, start)
	duration := int64(412)
	events := []contract.Event{
		platformEvent(1, start, "step", "platform-health-01-v1", "Node step started."),
		platformEvent(2, start.Add(time.Second), "log", "platform-health-01-v1", "Health observation"),
		platformEvent(3, start.Add(2*time.Second), "step", "platform-container-02-v1", "Node step started."),
		platformEvent(4, start.Add(3*time.Second), "log", "platform-container-02-v1", "Traffic switch acknowledged"),
		platformEvent(5, start.Add(4*time.Second), "step", "platform-cleanup-01-v1", "Node step started."),
		platformEvent(6, start.Add(5*time.Second), "log", "platform-cleanup-01-v1", "cleanup completed"),
		platformEvent(7, start.Add(6*time.Second), "log", "deploy", "Deployment succeeded"),
		platformEvent(8, start.Add(7*time.Second), "state", "", "succeeded"),
	}
	events[1].Health = &contract.HealthObservation{Target: "http://127.0.0.1:30000/healthz", Type: "http", Passed: true, Checks: 4, TimeoutSeconds: 30, Detail: "http status 200"}
	events[1].Step = "platform-health-01-v1"
	events[3].ReleaseID = "rel_00000000000000000000000000000001"
	events[3].ServingReleaseID = "rel_00000000000000000000000000000001"
	events[5].Step = "platform-cleanup-01-v1"
	events[5].DurationMS = &duration
	events[6].ReleaseID = "rel_00000000000000000000000000000001"
	events[6].ServingReleaseID = "rel_00000000000000000000000000000001"
	events[7].State = contract.Succeeded
	events[7].ServingReleaseID = "rel_00000000000000000000000000000001"
	if err := model.Page(events, start.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	if model.Serving.ReleaseID != "rel_00000000000000000000000000000001" {
		t.Fatalf("serving release was not taken from structured events: %+v", model.Serving)
	}
	if line := model.ServingLine(); !strings.Contains(line, "rel_00000000000000000000000000000001") {
		t.Fatalf("serving line does not name the release: %q", line)
	}
	if len(model.Health) != 1 || model.Health[0].Checks != 4 || model.Health[0].Target == "" || model.Health[0].TimeoutSeconds != 30 {
		t.Fatalf("structured health was not preserved: %+v", model.Health)
	}
	if !model.Cleanup.Observed || model.Cleanup.Status != StageCompleted || model.Cleanup.Duration != 412*time.Millisecond {
		t.Fatalf("cleanup was not visible: %+v", model.Cleanup)
	}
	if line := model.CleanupLine(); !strings.Contains(line, "completed") || !strings.Contains(line, "412ms") {
		t.Fatalf("cleanup line = %q", line)
	}
}

func TestStructuredHealthReplacesFailureWarning(t *testing.T) {
	id := contract.OperationID("op_" + strings.Repeat("b", 32))
	start := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	model := New(id, Options{})
	model.Snapshot(contract.Operation{
		ID: id, Kind: "platform.deploy", Target: "env/api/production",
		Actor: contract.Actor{Kind: "ci", Subject: "release"}, State: contract.Running,
		Attempt: 1, LastSequence: 2, CreatedAt: start, UpdatedAt: start,
	}, start)
	events := []contract.Event{
		platformEvent(1, start, "warning", "platform-health-02-v1", "Node step failed: health check did not pass"),
		platformEvent(2, start.Add(time.Second), "log", "platform-health-02-v1", "Health observation"),
	}
	events[1].Step = "platform-health-02-v1"
	events[1].Health = &contract.HealthObservation{Target: "http://127.0.0.1:20000/healthz", Passed: false, Checks: 60, TimeoutSeconds: 30, Detail: "health check did not pass within 30s"}
	if err := model.Page(events, start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(model.Health) != 1 || model.Health[0].Passed || model.Health[0].Checks != 60 || model.Health[0].Target == "" {
		t.Fatalf("structured failure did not replace the message-derived observation: %+v", model.Health)
	}
}

func TestServingLineIsHonestWithoutServingEvents(t *testing.T) {
	model := New("op_"+contract.OperationID(strings.Repeat("b", 32)), Options{})
	model.Kind = "platform.deploy"
	if line := model.ServingLine(); !strings.Contains(line, "unknown") || !strings.Contains(line, "no serving-container event") {
		t.Fatalf("serving line = %q", line)
	}
	model.Kind = "platform.app.create"
	if line := model.ServingLine(); !strings.Contains(line, "not applicable") {
		t.Fatalf("serving line = %q", line)
	}
}
