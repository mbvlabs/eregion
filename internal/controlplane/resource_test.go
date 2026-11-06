package controlplane

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/vault"
)

func TestResourceDeploymentDependencyCoversTargetAndPreviousRevision(t *testing.T) {
	dependency := resourceDeploymentDependency{
		RevisionID:        "erv_target",
		Resources:         []string{"cache"},
		PreviousResources: []string{"db"},
	}
	for _, name := range []string{"cache", "db"} {
		if !dependency.UsesResource(name) {
			t.Fatalf("queued/running deployment did not block its dependent resource %q", name)
		}
	}
	if dependency.UsesResource("queue") {
		t.Fatal("an unrelated resource was treated as a deployment dependency")
	}
	// A deployment row without a recorded revision cannot prove independence;
	// refuse the destroy rather than race an unknown switch.
	unknown := resourceDeploymentDependency{}
	if !unknown.UsesResource("anything") {
		t.Fatal("a deployment without a revision did not block an unknown dependency")
	}
}

func TestResourceSecretRevisionFingerprintIsDeterministicAndNonSecret(t *testing.T) {
	revisions := map[string]vault.RevisionRef{
		"DATABASE_PASSWORD": {
			Revision: 4, Hash: strings.Repeat("a", 64), KeyID: "key-1",
			CreatedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		},
		"DATABASE_URL": {
			Revision: 2, Hash: strings.Repeat("b", 64), KeyID: "key-1",
			CreatedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		},
	}
	first := resourceSecretRevisionFingerprint(revisions)
	second := resourceSecretRevisionFingerprint(revisions)
	if first == "" || first != second {
		t.Fatalf("fingerprint is not deterministic: %q %q", first, second)
	}
	if strings.Contains(first, "DATABASE_PASSWORD") || strings.Contains(first, strings.Repeat("a", 64)) {
		t.Fatalf("fingerprint contains a secret identity in plain text: %s", first)
	}
	revisions["DATABASE_PASSWORD"] = vault.RevisionRef{
		Revision: 5, Hash: strings.Repeat("c", 64), KeyID: "key-1",
		CreatedAt: time.Date(2026, 11, 6, 12, 5, 0, 0, time.UTC),
	}
	if resourceSecretRevisionFingerprint(revisions) == first {
		t.Fatal("a rotated secret revision did not change the fingerprint")
	}
}
