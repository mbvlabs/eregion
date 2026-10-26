package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

var followTestID = contract.OperationID("op_" + strings.Repeat("b", 32))

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
