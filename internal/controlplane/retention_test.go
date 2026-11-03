package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestRefusePrunedRelease is the acceptance check for D30's rollback refusal:
// a pruned release names itself and its prune time, returns precondition_failed
// and tells the operator to deploy a new build.
func TestRefusePrunedRelease(t *testing.T) {
	release := platform.Release{
		ID:          "rel_0123456789abcdef0123456789abcdef",
		App:         "api",
		Environment: "production",
		Status:      platform.ReleaseReady,
	}
	if err := refusePrunedRelease(release); err != nil {
		t.Fatalf("an unpruned release was refused: %v", err)
	}

	prunedAt := time.Date(2026, 11, 3, 10, 30, 0, 0, time.UTC)
	release.PrunedAt = &prunedAt
	err := refusePrunedRelease(release)
	if err == nil {
		t.Fatal("a pruned release was accepted")
	}
	fault := contract.Classify(err)
	if fault.Code != contract.PreconditionFailed {
		t.Fatalf("code = %s, want %s", fault.Code, contract.PreconditionFailed)
	}
	for _, want := range []string{"rel_0123456789abcdef0123456789abcdef", "api/production", "2026-11-03T10:30:00Z"} {
		if !strings.Contains(fault.Message, want) {
			t.Fatalf("message %q does not name %q", fault.Message, want)
		}
	}
	if !strings.Contains(strings.ToLower(fault.Hint), "deploy a new build") {
		t.Fatalf("hint %q does not tell the operator to deploy a new build", fault.Hint)
	}
}
