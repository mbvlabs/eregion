package schema_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/cli"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compile(t *testing.T, fragment string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("eregion-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("eregion-v1.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("eregion-v1.json" + fragment)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestCLIEnvelopes(t *testing.T) {
	schema := compile(t, "")
	for _, args := range [][]string{
		{"--json"}, {"version", "--json"}, {"commands", "--json"}, {"help", "version", "--json"},
		{"--help", "--json"}, {"unknown", "--json"}, {"--bad", "--json"}, {"--json", "--output"},
	} {
		var out bytes.Buffer
		cli.Run(context.Background(), args, strings.NewReader(""), &out, io.Discard)
		var value any
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
	}
}

func TestStreamRecords(t *testing.T) {
	schema := compile(t, "")
	var out bytes.Buffer
	stream := output.NewStream(&out, &secrets.Registry{})
	event := contract.Event{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Sequence: 1, At: time.Now().UTC(), Kind: "state", State: contract.Queued}
	if err := stream.Event(event); err != nil {
		t.Fatal(err)
	}
	if err := stream.Finish(contract.Success("eregion operations follow", map[string]any{"state": "queued"}, nil)); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	for range 2 {
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSchemaRejectsMalformedContracts(t *testing.T) {
	schema := compile(t, "")
	for _, raw := range []string{
		`{"schema":"eregion/v2","ok":true,"command":"eregion"}`,
		`{"schema":"eregion/v1","ok":false,"command":"eregion"}`,
		`{"schema":"eregion/v1","ok":true,"command":"eregion","error":{"code":"usage_error","message":"bad"}}`,
		`{"schema":"eregion/v1","ok":false,"command":"eregion","error":{"code":"invented","message":"bad"}}`,
		`{"schema":"eregion/v1","type":"event","event":{"operation_id":"op_demo","sequence":1}}`,
	} {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		if schema.Validate(value) == nil {
			t.Fatalf("schema accepted malformed contract: %s", raw)
		}
	}
}

func TestOperationAndErrorDefinitions(t *testing.T) {
	opSchema := compile(t, "#/$defs/operation")
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	op := contract.Operation{ID: id, Kind: "test", Target: "local", Actor: contract.Actor{Kind: "system", Subject: "test"}, State: contract.Queued, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(op)
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if err := opSchema.Validate(value); err != nil {
		t.Fatal(err)
	}
	codeSchema := compile(t, "#/$defs/code")
	for _, code := range []contract.Code{contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed, contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.PreconditionFailed, contract.ConfirmationRequired, contract.Conflict, contract.Unavailable, contract.Cancelled, contract.Internal} {
		if err := codeSchema.Validate(string(code)); err != nil {
			t.Fatal(err)
		}
	}
}
