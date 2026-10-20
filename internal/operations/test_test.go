package operations

import (
	"encoding/json"
	"testing"
)

func TestDiagnosticInput(t *testing.T) {
	input, err := NormalizeTestInput(nil)
	if err != nil || input.Steps != 3 || input.StepDelayMS != 100 {
		t.Fatal("incorrect defaults")
	}
	for _, raw := range []string{`null`, `[]`, `{`, `{} {}`, `{"steps":0}`, `{"steps":21}`, `{"step_delay_ms":1001}`, `{"fail_until_attempt":4}`, `{"credentials":"no"}`} {
		if _, err := NormalizeTestInput(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
