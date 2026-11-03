package controlplane

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestContainerCleanupBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: time.Minute},
		{attempts: 1, want: time.Minute},
		{attempts: 2, want: 2 * time.Minute},
		{attempts: 3, want: 4 * time.Minute},
		{attempts: 6, want: 32 * time.Minute},
		{attempts: 7, want: time.Hour},
		{attempts: 20, want: time.Hour},
	}
	for _, test := range tests {
		if got := containerCleanupBackoff(test.attempts); got != test.want {
			t.Errorf("containerCleanupBackoff(%d) = %s, want %s", test.attempts, got, test.want)
		}
	}
}

func TestCleanupOperationIDIsStablePerLeak(t *testing.T) {
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	row := containerCleanupRow{ContainerName: "eregion-demo-production-web-1", CreatedAt: created}
	first := cleanupOperationID(row)
	if second := cleanupOperationID(row); second != first {
		t.Fatalf("cleanup operation ID changed: %s != %s", first, second)
	}
	row.CreatedAt = created.Add(time.Second)
	if again := cleanupOperationID(row); again == first {
		t.Fatal("a new leak reused the old cleanup operation ID")
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("cleanup operation ID is invalid: %v", err)
	}
	if got := containerCleanupBackoff(1); got != time.Minute {
		t.Fatalf("first cleanup retry = %s", got)
	}
}

// A failed removal must name its containers and the node's reason in the
// cleanup event; joined step errors alone classify as an internal error.
func TestContainerCleanupErrorNamesFailedContainersAndCause(t *testing.T) {
	step := errors.New("exit status 1: simulated removal failure")
	err := containerCleanupError([]string{"demo-production-web-1-aaaa", "demo-production-web-2-aaaa"},
		[]string{"exit status 1: Error response from daemon: removal of container demo-production-web-1-aaaa is already in progress"},
		[]error{step, step})
	fault := contract.Classify(err)
	if fault.Code != contract.Unavailable {
		t.Fatalf("code = %s, want %s", fault.Code, contract.Unavailable)
	}
	for _, want := range []string{
		"2 container(s) could not be removed", "demo-production-web-1-aaaa", "demo-production-web-2-aaaa",
		"Error response from daemon: removal of container demo-production-web-1-aaaa is already in progress",
	} {
		if !strings.Contains(fault.Message, want) {
			t.Fatalf("message %q does not contain %q", fault.Message, want)
		}
	}
	if !errors.Is(err, step) {
		t.Fatal("the step error is no longer wrapped")
	}
	if containerCleanupError(nil, nil, nil) != nil {
		t.Fatal("no failure must return nil")
	}
}

// The warning keeps the node's own step reason, not the generic internal text.
func TestCleanupStepCauseNamesTheStepReason(t *testing.T) {
	service := &Service{}
	step := "platform-cleanup-demo-production-web-1-aaaa-v1"
	retryable := fmt.Errorf("node step %s failed: %s", step, "exit status 1: Error response from daemon: permission denied")
	if got := service.cleanupStepCause(retryable, step); got != "exit status 1: Error response from daemon: permission denied" {
		t.Fatalf("retryable cause = %q", got)
	}
	permanent := errors.Join(errNodeStepPermanentFailure,
		contract.NewFault(contract.PreconditionFailed, "node step failed: exit status 1: container is marked for removal", "inspect the node"))
	if got := service.cleanupStepCause(permanent, step); got != "exit status 1: container is marked for removal" {
		t.Fatalf("permanent cause = %q", got)
	}
}

// A Docker answer is untrusted terminal text: it must not carry controls or
// newlines into an event, and it must stay one bounded line.
func TestBoundedCleanupCauseSanitizesAndTruncates(t *testing.T) {
	long := strings.Repeat("x", cleanupCauseLimit+40)
	got := boundedCleanupCause("\x1b[31mError\x1b[0m\r\nresponse\tfrom daemon: " + long)
	if strings.ContainsAny(got, "\x1b\r\n") {
		t.Fatalf("cause kept control characters: %q", got)
	}
	if !strings.Contains(got, "Error") || !strings.Contains(got, "response from daemon:") {
		t.Fatalf("cause lost the docker reason: %q", got)
	}
	if runes := []rune(got); len(runes) != cleanupCauseLimit+1 || runes[len(runes)-1] != '…' {
		t.Fatalf("bounded cause has %d runes, want %d plus an ellipsis", len(runes), cleanupCauseLimit)
	}
	if got := boundedCleanupCause("   "); got != "" {
		t.Fatalf("blank cause = %q", got)
	}
}

// The maintenance retry records the node's removal reason, not just the
// generic failure text, and bounds it the same way.
func TestContainerCleanupFailureIncludesTheNodeReason(t *testing.T) {
	service := &Service{}
	fault := contract.Classify(service.containerCleanupFailure(contract.Internal, "exit status 1: Error response from daemon: device or resource busy"))
	if !strings.Contains(fault.Message, "Error response from daemon: device or resource busy") {
		t.Fatalf("maintenance detail = %q", fault.Message)
	}
	if unknown := contract.Classify(service.containerCleanupFailure(contract.Code("nonsense"), "")); unknown.Code != contract.Internal {
		t.Fatalf("unknown code = %s", unknown.Code)
	}
}

// assertNoSecretFragment fails when any short window of secret survives, so a
// cause that was truncated before redaction cannot pass by exposing a prefix.
func assertNoSecretFragment(t *testing.T, text, secret string) {
	t.Helper()
	window := 8
	if len(secret) < window {
		window = len(secret)
	}
	for i := 0; i+window <= len(secret); i++ {
		if fragment := secret[i : i+window]; strings.Contains(text, fragment) {
			t.Fatalf("cause %q exposes secret fragment %q", text, fragment)
		}
	}
}

// The registry matches exact values, so a secret that crosses the truncation
// boundary must be redacted from the raw cause before it is shortened.
func TestCleanupCausesRedactSecretsStraddlingTheTruncationBoundary(t *testing.T) {
	secret := "sk_live_" + strings.Repeat("9f8e7d6c", 6)
	registry := &secrets.Registry{}
	registry.Add(secret)
	service := &Service{redaction: registry}
	step := "platform-cleanup-demo-production-web-1-aaaa-v1"
	base := "exit status 1: Error response from daemon: "
	filler := strings.Repeat("x", cleanupCauseLimit-len(base)-24)
	raw := base + filler + secret + " trailing text"
	// Guard the test setup: shortening first would expose the secret's prefix.
	if leaked := boundedCleanupCause(raw); !strings.Contains(leaked, secret[:8]) {
		t.Fatalf("setup does not straddle the boundary: %q", leaked)
	}
	err := fmt.Errorf("node step %s failed: %s", step, raw)
	cause := service.cleanupStepCause(err, step)
	assertNoSecretFragment(t, cause, secret)
	if !strings.Contains(cause, "[REDACTED") {
		t.Fatalf("cause does not show the redaction: %q", cause)
	}
	assertNoSecretFragment(t, service.cleanupCauseText(err), secret)
	maintenance := contract.Classify(service.containerCleanupFailure(contract.Internal, raw))
	assertNoSecretFragment(t, maintenance.Message, secret)
	warning := contract.Classify(containerCleanupError([]string{"web-1"}, []string{cause}, []error{err}))
	assertNoSecretFragment(t, warning.Message, secret)
}

// A secret containing newlines (or sitting between them) is only an exact
// registry match before whitespace normalization collapses it.
func TestCleanupCausesRedactSecretsAroundNewlines(t *testing.T) {
	secret := "multi\nline-secret-value"
	registry := &secrets.Registry{}
	registry.Add(secret)
	service := &Service{redaction: registry}
	step := "platform-cleanup-demo-production-web-1-aaaa-v1"
	raw := "\n" + secret + "\nsecond line"
	err := fmt.Errorf("node step %s failed: %s", step, raw)
	cause := service.cleanupStepCause(err, step)
	assertNoSecretFragment(t, cause, secret)
	assertNoSecretFragment(t, service.cleanupCauseText(fmt.Errorf("node step %s failed: %s", step, raw)), secret)
	maintenance := contract.Classify(service.containerCleanupFailure(contract.Internal, "exit status 1: "+raw))
	assertNoSecretFragment(t, maintenance.Message, secret)
	if strings.Contains(cause, "line-secret-value") || strings.Contains(maintenance.Message, "line-secret-value") {
		t.Fatalf("newline secret survived: cause=%q maintenance=%q", cause, maintenance.Message)
	}
}
