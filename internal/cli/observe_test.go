package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func TestWriteEnvironmentObservationReportsPendingCleanup(t *testing.T) {
	observation := platform.EnvironmentObservation{Environment: platform.Environment{
		App: "demo", Name: "production",
		PendingCleanup: []string{"eregion-demo-production-web-1", "eregion-demo-production-worker-1"},
	}}
	plain := writeEnvironmentObservation(observation)
	if !strings.Contains(plain, "2 leaked containers pending cleanup") {
		t.Fatalf("status line missing from output:\n%s", plain)
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"pending_cleanup":["eregion-demo-production-web-1","eregion-demo-production-worker-1"]`) {
		t.Fatalf("JSON did not name the pending containers: %s", raw)
	}
}
