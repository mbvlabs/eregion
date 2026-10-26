package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
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
	flags := []string{"idempotency-key", "poll-interval"}
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
