package rpc

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestRequestRoundTripAndBound(t *testing.T) {
	request := Request{
		Schema: Schema, Method: "operations.get", Params: json.RawMessage(`{"operation_id":"op_x"}`),
		Operator: "admin", ClientVersion: "v1", ClientCommit: "abc",
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeRequest(bytes.NewReader(append(raw, '\n')))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Method != request.Method || decoded.Operator != request.Operator || !ValidMethod(decoded.Method) || !requestIDPattern.MatchString(decoded.RequestID) {
		t.Fatalf("unexpected request: %+v", decoded)
	}

	tooLarge := `{"schema":"` + Schema + `","method":"version","params":{"x":"` + strings.Repeat("a", MaxRequestBytes) + `"}}`
	if _, err := DecodeRequest(strings.NewReader(tooLarge)); err == nil {
		t.Fatal("oversized request accepted")
	}
	if _, err := DecodeRequest(strings.NewReader(string(raw) + string(raw))); err == nil {
		t.Fatal("two requests accepted")
	}
	if _, err := DecodeRequest(strings.NewReader(`{"schema":"other","method":"version"}`)); err == nil {
		t.Fatal("wrong schema accepted")
	}
	if _, err := DecodeRequest(strings.NewReader(`{"schema":"` + Schema + `","method":"not a method"}`)); err == nil {
		t.Fatal("malformed method accepted")
	}
}

func TestResultEncoding(t *testing.T) {
	raw, err := MarshalResult(Result{OK: false, Error: contract.NewFault(contract.NodeVersionMismatch, "mismatch", "upgrade")})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != Schema || decoded.Type != "result" || decoded.OK || decoded.Error == nil || decoded.Error.Code != contract.NodeVersionMismatch {
		t.Fatalf("unexpected result: %+v", decoded)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatal("result is not newline delimited")
	}
}

func TestSameBuild(t *testing.T) {
	if !SameBuild(Build{Version: "v1.0.0", Commit: "a"}, Build{Version: "v1.0.0", Commit: "b"}) {
		t.Fatal("release builds with equal versions must match")
	}
	if SameBuild(Build{Version: "v1.0.0", Commit: "a"}, Build{Version: "v1.0.1", Commit: "a"}) {
		t.Fatal("different versions matched")
	}
	if SameBuild(Build{Version: "dev", Commit: "a"}, Build{Version: "dev", Commit: "b"}) {
		t.Fatal("dev builds with different commits matched")
	}
	if !SameBuild(Build{Version: "dev", Commit: "a"}, Build{Version: "dev", Commit: "a"}) {
		t.Fatal("equal dev builds did not match")
	}
}
