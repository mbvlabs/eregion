package contract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIDsAndKeys(t *testing.T) {
	seen := map[OperationID]bool{}
	for range 100 {
		id, err := NewOperationID()
		if err != nil || id.Validate() != nil || seen[id] {
			t.Fatalf("invalid/duplicate ID: %s %v", id, err)
		}
		seen[id] = true
	}
	for _, id := range []OperationID{"", "op_demo", OperationID("op_" + strings.Repeat("A", 32)), OperationID("op_" + strings.Repeat("0", 33))} {
		if id.Validate() == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	for _, key := range []string{"ci:build_123.release-1", strings.Repeat("x", 128)} {
		if err := ValidateIdempotencyKey(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", "has spaces", "ümlaut", strings.Repeat("x", 129), "\n"} {
		if ValidateIdempotencyKey(key) == nil {
			t.Fatalf("accepted key %q", key)
		}
	}
}

func TestTransitions(t *testing.T) {
	allowed := map[[2]State]bool{
		{Queued, Running}: true, {Queued, Cancelling}: true, {Queued, Failed}: true,
		{Running, Cancelling}: true, {Running, Succeeded}: true, {Running, Failed}: true,
		{Cancelling, Canceled}: true, {Cancelling, Succeeded}: true, {Cancelling, Failed}: true,
	}
	states := []State{Queued, Running, Cancelling, Succeeded, Failed, Canceled, "unknown"}
	for _, from := range states {
		for _, to := range states {
			want := allowed[[2]State{from, to}]
			if CanTransition(from, to) != want || (ValidateTransition(from, to) == nil) != want {
				t.Fatalf("incorrect transition %s -> %s", from, to)
			}
		}
	}
}

func sampleEvent() Event {
	return Event{OperationID: "op_" + OperationID(strings.Repeat("a", 32)), Sequence: 1, At: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC), Kind: "state", State: Queued}
}

func TestEventValidation(t *testing.T) {
	if err := sampleEvent().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Event){
		func(e *Event) { e.OperationID = "bad" },
		func(e *Event) { e.Sequence = 0 },
		func(e *Event) { e.At = time.Time{} },
		func(e *Event) { e.Kind = "unknown" },
		func(e *Event) { e.State = "unknown" },
		func(e *Event) { e.Kind = "log" },
		func(e *Event) { e.Kind = "step"; e.State = "" },
		func(e *Event) { e.Kind = "progress"; e.State = "" },
		func(e *Event) { e.Progress = &Progress{Current: 1, Total: 2, Unit: "bytes"} },
	} {
		e := sampleEvent()
		change(&e)
		if e.Validate() == nil {
			t.Fatalf("accepted invalid event: %+v", e)
		}
	}
	e := sampleEvent()
	e.Kind, e.State = "progress", ""
	e.Progress = &Progress{Current: 2, Total: 1, Unit: "bytes"}
	if e.Validate() == nil {
		t.Fatal("accepted progress past total")
	}
	e.Progress = &Progress{Current: 0, Total: 1, Unit: "bytes"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	step := sampleEvent()
	step.Kind, step.State, step.Stage = "step", "", "diagnostic-step-01-v1"
	step.ExecutionID = string(step.OperationID) + ":1:diagnostic-step-01-v1"
	if err := step.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := step
	bad.ExecutionID = "not-an-execution"
	if bad.Validate() == nil {
		t.Fatal("accepted invalid execution identity")
	}
	bad = step
	bad.Kind, bad.ExecutionID = "log", step.ExecutionID
	if bad.Validate() == nil {
		t.Fatal("only step events may carry an execution identity")
	}
}

func TestStructuredPlatformEventValidation(t *testing.T) {
	event := sampleEvent()
	event.Kind, event.State = "log", ""
	event.Step = "platform-health-01-v1"
	event.ReleaseID = "rel_" + strings.Repeat("b", 32)
	event.ServingReleaseID = "rel_" + strings.Repeat("c", 32)
	duration := int64(12)
	event.DurationMS = &duration
	event.Health = &HealthObservation{Target: "http://127.0.0.1:20000/", Type: "http", Passed: true, Checks: 2, TimeoutSeconds: 30, Detail: "http status 200"}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"step":"platform-health-01-v1"`, `"release_id":"rel_`, `"serving_release_id":"rel_`, `"duration_ms":12`, `"health":{`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("structured field %s is not on the wire: %s", want, raw)
		}
	}
	for name, change := range map[string]func(*Event){
		"bad release":         func(e *Event) { e.ReleaseID = "release-1" },
		"bad serving release": func(e *Event) { e.ServingReleaseID = "rel_x" },
		"bad step":            func(e *Event) { e.Step = "two words" },
		"negative duration":   func(e *Event) { value := int64(-1); e.DurationMS = &value },
		"negative checks":     func(e *Event) { e.Health.Checks = -1 },
		"control health":      func(e *Event) { e.Health.Detail = "line\nbreak" },
	} {
		bad := event
		health := *event.Health
		bad.Health = &health
		change(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s: accepted invalid structured event", name)
		}
	}
}

func TestOperationErrorCodeIsMachineReadable(t *testing.T) {
	operation := Operation{
		ID: "op_" + OperationID(strings.Repeat("a", 32)), Kind: "diagnostic.test", Target: "control-plane",
		Actor: Actor{Kind: "ci", Subject: "release"}, State: Failed, Attempt: 1, LastSequence: 2,
		ErrorCode: PreconditionFailed,
		CreatedAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 20, 12, 0, 1, 0, time.UTC),
	}
	if err := operation.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"error_code":"precondition_failed"`) {
		t.Fatalf("the error code is not on the wire: %s", raw)
	}
	// Only a failed operation carries an error code.
	succeeded := operation
	succeeded.State, succeeded.ErrorCode = Succeeded, PreconditionFailed
	if succeeded.Validate() == nil {
		t.Fatal("a succeeded operation carried an error code")
	}
	unknown := operation
	unknown.ErrorCode = "made_up"
	if unknown.Validate() == nil {
		t.Fatal("an unknown error code was accepted")
	}
	// A failed operation without an error code stays valid: the code is
	// additive, not required.
	withoutCode := operation
	withoutCode.ErrorCode = ""
	if err := withoutCode.Validate(); err != nil {
		t.Fatal(err)
	}
	// A create may expose the deploy it queued in the same transaction. The
	// field is optional and must be a real operation ID when present.
	first := operation
	first.FirstDeployOperationID = OperationID("op_" + strings.Repeat("b", 32))
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(first)
	if err != nil || !strings.Contains(string(raw), `"first_deploy_operation_id":"op_`) {
		t.Fatalf("first deploy identity is not on the wire: %s %v", raw, err)
	}
	badFirst := first
	badFirst.FirstDeployOperationID = "deploy-1"
	if badFirst.Validate() == nil {
		t.Fatal("an invalid first deploy operation ID was accepted")
	}
}

func TestEventErrorCodeOnlyOnAFailedState(t *testing.T) {
	failed := sampleEvent()
	failed.Kind, failed.State, failed.ErrorCode = "state", Failed, PreconditionFailed
	if err := failed.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"error_code":"precondition_failed"`) {
		t.Fatalf("the error code is not on the wire: %s", raw)
	}
	running := sampleEvent()
	running.Kind, running.State, running.ErrorCode = "state", Running, PreconditionFailed
	if running.Validate() == nil {
		t.Fatal("a running state event carried an error code")
	}
	log := sampleEvent()
	log.Kind, log.State, log.ErrorCode = "log", "", PreconditionFailed
	if log.Validate() == nil {
		t.Fatal("a log event carried an error code")
	}
	unknown := sampleEvent()
	unknown.Kind, unknown.State, unknown.ErrorCode = "state", Failed, "made_up"
	if unknown.Validate() == nil {
		t.Fatal("an unknown error code was accepted")
	}
}

func TestSequencesAreJSONStringIntegers(t *testing.T) {
	event := sampleEvent()
	event.Sequence = 18446744073709551615
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sequence":"18446744073709551615"`) {
		t.Fatal(string(raw))
	}
	var decoded Event
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Sequence != event.Sequence {
		t.Fatalf("lost event sequence: %v %+v", err, decoded)
	}
}

func TestActor(t *testing.T) {
	for _, actor := range []Actor{{Kind: "ci", Subject: "release"}, {Kind: "system", Subject: "scheduler"}} {
		if err := actor.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, actor := range []Actor{{}, {Kind: "user", Subject: "me"}, {Kind: "ci", Subject: " "}, {Kind: "ci", Subject: strings.Repeat("x", 257)}} {
		if actor.Validate() == nil {
			t.Fatalf("accepted %+v", actor)
		}
	}
}

func TestConfirmationBinding(t *testing.T) {
	binding := PlanBinding{Actor: Actor{Kind: "operator", Subject: "alice"}, Action: "rollback", Target: "env_1", BaseRevision: "rev_1", InputsHash: strings.Repeat("a", 64), ExpiresAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	digest, err := binding.Digest()
	if err != nil || len(digest) != 64 {
		t.Fatalf("%q %v", digest, err)
	}
	otherZone := binding
	otherZone.ExpiresAt = binding.ExpiresAt.In(time.FixedZone("offset", 3600))
	if other, _ := otherZone.Digest(); other != digest {
		t.Fatal("same instant produced different bindings")
	}
	for _, change := range []func(*PlanBinding){
		func(b *PlanBinding) { b.Actor.Subject = "bob" },
		func(b *PlanBinding) { b.Action = "restore" },
		func(b *PlanBinding) { b.Target = "env_2" },
		func(b *PlanBinding) { b.BaseRevision = "rev_2" },
		func(b *PlanBinding) { b.InputsHash = strings.Repeat("b", 64) },
		func(b *PlanBinding) { b.ExpiresAt = b.ExpiresAt.Add(time.Second) },
	} {
		changed := binding
		change(&changed)
		if other, err := changed.Digest(); err != nil || other == digest {
			t.Fatalf("binding did not change: %+v %v", changed, err)
		}
	}
	binding.InputsHash = "not-a-sha256"
	if _, err := binding.Digest(); err == nil {
		t.Fatal("accepted invalid input hash")
	}
}

func TestFaultsAndExitCodes(t *testing.T) {
	for code, expected := range exits {
		err := NewFault(code, "safe message", "safe hint")
		if !KnownCode(code) || ExitCode(err) != expected {
			t.Fatalf("unstable code %s", code)
		}
	}
	if ExitCode(nil) != 0 || ExitCode(errors.New("secret internal failure")) != 70 || ExitCode(context.Canceled) != 130 || ExitCode(context.DeadlineExceeded) != 6 {
		t.Fatal("incorrect generic/cancellation exit code")
	}
	classified := Classify(errors.New("secret internal failure"))
	raw, _ := json.Marshal(classified)
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("exposed implementation error: %s", raw)
	}
	if Classify(NewFault("unknown", "untrusted", "")).Code != Internal {
		t.Fatal("unknown error code escaped the contract")
	}
}

func TestEnvelopeValidation(t *testing.T) {
	for _, envelope := range []Envelope{
		Success("eregion version", nil, nil),
		Failure("eregion", NewFault(Usage, "invalid input", "Inspect help.")),
	} {
		if err := envelope.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, envelope := range []Envelope{
		{},
		{Schema: Schema, OK: true, Command: "eregion", Error: NewFault(Usage, "bad", "")},
		{Schema: Schema, Command: "eregion"},
		{Schema: Schema, Command: "eregion", Error: NewFault("unknown", "bad", "")},
		{Schema: Schema, Command: "eregion", Error: NewFault(Usage, "", "")},
		{Schema: Schema, Command: "eregion", Data: "unexpected", Error: NewFault(Usage, "bad", "")},
		Success("eregion", nil, []string{" "}),
	} {
		if envelope.Validate() == nil {
			t.Fatalf("accepted invalid result: %+v", envelope)
		}
	}
}
