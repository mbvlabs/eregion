package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

var followTestID = contract.OperationID("op_" + strings.Repeat("b", 32))

func followTestOperation(state contract.State) contract.Operation {
	created := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return contract.Operation{
		ID:     followTestID,
		Kind:   "diagnostic.test",
		Target: "control-plane",
		Actor:  contract.Actor{Kind: "ci", Subject: "release"},
		State:  state, Attempt: 1, LastSequence: 3,
		CreatedAt: created, UpdatedAt: created.Add(3 * time.Second),
	}
}

func followTestEvents() []contract.Event {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	return []contract.Event{
		{OperationID: followTestID, Sequence: 1, At: at, Kind: "state", State: contract.Queued, Message: "accepted durably"},
		{OperationID: followTestID, Sequence: 2, At: at.Add(time.Second), Kind: "step", Attempt: 1, Stage: "preflight", Message: "step started"},
		{OperationID: followTestID, Sequence: 3, At: at.Add(2 * time.Second), Kind: "state", Attempt: 1, State: contract.Succeeded, Message: "operation succeeded"},
	}
}

// operationAPI is a minimal authenticated API double. It serves the existing
// envelope and event-page contracts; it does not simulate control-plane work.
type operationAPI struct {
	id          contract.OperationID
	operation   contract.Operation
	events      []contract.Event
	block       bool
	started     chan struct{}
	once        sync.Once
	mu          sync.Mutex
	cancelCalls int
}

func newOperationAPI(t *testing.T, state contract.State) (*operationAPI, *httptest.Server) {
	t.Helper()
	api := &operationAPI{
		id: followTestID, operation: followTestOperation(state), events: followTestEvents(),
		started: make(chan struct{}),
	}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	t.Setenv("EREGION_API_URL", server.URL)
	t.Setenv("EREGION_API_TOKEN", strings.Repeat("t", 32))
	return api, server
}

func (a *operationAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/cancel") {
		a.mu.Lock()
		a.cancelCalls++
		a.mu.Unlock()
		writeTestEnvelope(w, a.operation)
		return
	}
	if r.Method == http.MethodPost {
		writeTestEnvelope(w, a.operation)
		return
	}
	if a.block {
		a.once.Do(func() { close(a.started) })
		<-r.Context().Done()
		return
	}
	prefix := "/v1/operations/" + string(a.id)
	switch {
	case r.URL.Path == prefix:
		writeTestEnvelope(w, a.operation)
	case r.URL.Path == prefix+"/events":
		after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 200
		}
		page := struct {
			Events []contract.Event `json:"events"`
			Cursor contract.Cursor  `json:"cursor"`
		}{Events: []contract.Event{}, Cursor: contract.Cursor{OperationID: a.id, After: after}}
		for _, event := range a.events {
			if event.Sequence <= after {
				continue
			}
			page.Events = append(page.Events, event)
			page.Cursor.After = event.Sequence
			if len(page.Events) == limit {
				break
			}
		}
		writeTestEnvelope(w, page)
	default:
		http.NotFound(w, r)
	}
}

func writeTestEnvelope(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(contract.Success("api.test", data, nil))
}

func followArgs(extra ...string) []string {
	return append([]string{"operations", "follow", string(followTestID), "--poll-interval", "1ms"}, extra...)
}

func TestFollowPlainOutput(t *testing.T) {
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, followArgs()...)
	if code != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("plain follow wrote stderr: %q", stderr)
	}
	for _, want := range []string{"accepted durably", "step preflight", "succeeded " + string(followTestID), "3 events"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("plain output missing %q:\n%s", want, stdout)
		}
	}
	if strings.ContainsRune(stdout, '\x1b') {
		t.Fatalf("plain output contains terminal escapes: %q", stdout)
	}
}

func TestFollowJSONOutput(t *testing.T) {
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, followArgs("--json")...)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("follow failed: %+v", envelope.Error)
	}
	raw, _ := json.Marshal(envelope.Data)
	var data struct {
		State    contract.State     `json:"state"`
		Events   uint64             `json:"events"`
		Cursor   uint64             `json:"cursor,string"`
		Detached bool               `json:"detached"`
		Op       contract.Operation `json:"operation"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.State != contract.Succeeded || data.Events != 3 || data.Cursor != 3 || data.Detached || data.Op.ID != followTestID {
		t.Fatalf("unexpected final result: %s", raw)
	}
}

func TestFollowNDJSONOutput(t *testing.T) {
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, followArgs("--output", "ndjson")...)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var records []output.Record
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		var record output.Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid NDJSON record %q: %v", line, err)
		}
		records = append(records, record)
	}
	if len(records) != 4 {
		t.Fatalf("expected 3 events and one result, got %d records:\n%s", len(records), stdout)
	}
	for i, record := range records[:3] {
		if record.Type != "event" || record.Event == nil || record.Event.Sequence != uint64(i+1) {
			t.Fatalf("record %d is not the expected event: %+v", i, record)
		}
	}
	last := records[3]
	if last.Type != "result" || last.Result == nil || !last.Result.OK {
		t.Fatalf("missing terminal result record: %+v", last)
	}
	raw, _ := json.Marshal(last.Result.Data)
	if !strings.Contains(string(raw), `"state":"succeeded"`) {
		t.Fatalf("result record lacks the final state: %s", raw)
	}
}

func TestFollowInteractiveRequiresATerminal(t *testing.T) {
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, followArgs("--interactive")...)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "interactive viewer requires a terminal") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestFollowInteractiveIsIgnoredByMachineOutput(t *testing.T) {
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, followArgs("--interactive", "--json")...)
	if code != 0 || stderr != "" || !decodeOne(t, stdout).OK {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestFollowDetachDoesNotCancel(t *testing.T) {
	api, _ := newOperationAPI(t, contract.Running)
	api.block = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, followArgs(), panicReader{}, &stdout, &stderr, &secrets.Registry{})
	}()
	select {
	case <-api.started:
	case <-time.After(5 * time.Second):
		t.Fatal("follow never contacted the API")
	}
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("detach exit code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not stop after cancellation")
	}
	if !strings.Contains(stdout.String(), string(followTestID)) || !strings.Contains(stdout.String(), "resume: eregion operations follow") {
		t.Fatalf("detach summary lacks identity/resume: %q", stdout.String())
	}
	api.mu.Lock()
	cancels := api.cancelCalls
	api.mu.Unlock()
	if cancels != 0 {
		t.Fatalf("detaching called cancel %d times", cancels)
	}
}

func TestNDJSONIsRejectedForOrdinaryCommands(t *testing.T) {
	code, stdout, stderr := invoke(t, "operations", "get", string(followTestID), "--output", "ndjson")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "invalid output mode") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestFollowMetadataAndFlags(t *testing.T) {
	code, stdout, _ := invoke(t, "commands", "--path", "operations follow", "--json")
	if code != 0 {
		t.Fatal(stdout)
	}
	envelope := decodeOne(t, stdout)
	raw, _ := json.Marshal(envelope.Data)
	var data struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Commands) != 1 || data.Commands[0].Output != "events" {
		t.Fatalf("follow metadata is wrong: %s", raw)
	}
	flags := map[string]bool{}
	for _, flag := range data.Commands[0].Flags {
		flags[flag.Name] = true
	}
	for _, name := range []string{"interactive", "ascii", "reduced-motion", "poll-interval", "output", "json"} {
		if !flags[name] {
			t.Fatalf("follow does not advertise --%s: %s", name, raw)
		}
	}
}

func TestFollowSelectionRecognizesOnlyFollowCommands(t *testing.T) {
	flags := []string{"api-url", "idempotency-key", "poll-interval"}
	for _, test := range []struct {
		args   []string
		active bool
		ndjson bool
	}{
		{[]string{"operations", "follow", "op_x", "--output", "ndjson"}, true, true},
		{[]string{"--output=ndjson", "operations", "follow", "op_x"}, true, true},
		{[]string{"operations", "get", "op_x", "--output", "ndjson"}, false, false},
		{[]string{"operations", "follow", "op_x", "--output", "json"}, true, false},
	} {
		selection := followSelectionFromArgs(test.args, flags)
		if selection.Active != test.active || selection.NDJSON != test.ndjson {
			t.Fatalf("%v: %+v", test.args, selection)
		}
	}
}

func TestFollowRejectsNonPositivePollInterval(t *testing.T) {
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, "operations", "follow", string(followTestID), "--poll-interval", "0s")
	if code != 2 || stdout != "" || !strings.Contains(stderr, "poll interval must be positive") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
