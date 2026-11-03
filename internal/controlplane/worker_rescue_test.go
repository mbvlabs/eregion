package controlplane

import (
	"strings"
	"testing"
	"time"
)

// TestShouldRescueJob proves the recovery decision uses only the last River
// attempt: an earlier same-host attempt is history, not an orphan.
func TestShouldRescueJob(t *testing.T) {
	const host = "node-a"
	prefix := workerHostPrefix(host)
	other := workerHostPrefix("node-b")
	newClient := newWorkerClientID(host, time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))

	tests := []struct {
		name        string
		attemptedBy []string
		want        bool
	}{
		{"last attempt on this host", []string{other + "old", prefix + "previous"}, true},
		{"new client excluded", []string{prefix + "previous", newClient}, false},
		{"other host excluded", []string{other + "current"}, false},
		{"last attempted_by only", []string{prefix + "previous", other + "current"}, false},
		{"empty history", nil, false},
		{"empty last attempt", []string{prefix + "previous", ""}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldRescueJob(test.attemptedBy, prefix, newClient); got != test.want {
				t.Fatalf("shouldRescueJob(%q) = %t, want %t", test.attemptedBy, got, test.want)
			}
		})
	}
}

// TestWorkerHostPrefixHasNoCollisions covers the names whose sanitisation used
// to merge or prefix-match: web/web-prod and web_a/web+a.
func TestWorkerHostPrefixHasNoCollisions(t *testing.T) {
	started := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct{ one, two string }{
		{"web", "web-prod"},
		{"web_a", "web+a"},
		{"web", "web"},
	} {
		onePrefix := workerHostPrefix(test.one)
		twoID := newWorkerClientID(test.two, started)
		if test.one == test.two {
			if !strings.HasPrefix(twoID, onePrefix) {
				t.Fatalf("%q does not use its own host prefix", test.one)
			}
			continue
		}
		if strings.HasPrefix(twoID, onePrefix) || strings.HasPrefix(onePrefix, workerHostPrefix(test.two)) {
			t.Fatalf("%q and %q share a client ID prefix: %q %q", test.one, test.two, onePrefix, workerHostPrefix(test.two))
		}
	}
}

// TestWorkerClientIDIsStableAndBounded keeps the rescue match stable across
// restarts and the ID within River's limit.
func TestWorkerClientIDIsStableAndBounded(t *testing.T) {
	started := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	if got, want := workerHostPrefix("Node-1.Example.com"), workerHostPrefix("node-1.example.com"); got != want {
		t.Fatalf("hostname case changed the prefix: %q != %q", got, want)
	}
	long := newWorkerClientID(strings.Repeat("h", 80), started)
	if len(long) > 100 {
		t.Fatalf("client ID %q is longer than River's 100-character limit", long)
	}
	if workerHostPrefix(strings.Repeat("h", 80)) == workerHostPrefix(strings.Repeat("h", 79)) {
		t.Fatal("long hostnames shared a prefix")
	}
}
