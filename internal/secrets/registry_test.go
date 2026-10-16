package secrets

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestRedaction(t *testing.T) {
	var registry Registry
	registry.Add("token", "token-with-suffix", "\"quoted\"\nsecret", "[REDACTED]", "", "xy")
	text := registry.Redact("token-with-suffix / token / \"quoted\"\nsecret / xy")
	if text != strings.Join([]string{Placeholder, Placeholder, Placeholder, Placeholder}, " / ") {
		t.Fatal(text)
	}
	raw, err := registry.JSON(map[string]any{
		"token-with-suffix": []any{"\"quoted\"\nsecret", "xy"},
		"integer":           uint64(18446744073709551615),
	})
	if err != nil || strings.Contains(string(raw), "suffix") || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "xy") {
		t.Fatalf("%s %v", raw, err)
	}
	if !strings.Contains(string(raw), "18446744073709551615") || !json.Valid(raw) {
		t.Fatalf("invalid or imprecise JSON: %s", raw)
	}
}

func TestSerializationFailureDoesNotReturnRawData(t *testing.T) {
	var registry Registry
	if raw, err := registry.JSON(make(chan string)); err == nil || raw != nil {
		t.Fatalf("%s %v", raw, err)
	}
}

func TestConcurrentRegistry(t *testing.T) {
	var registry Registry
	registry.Add("secret")
	var wait sync.WaitGroup
	for range 30 {
		wait.Go(func() {
			registry.Add("another-secret")
			if got := registry.Redact("secret"); got != Placeholder {
				t.Errorf("redaction failed: %q", got)
			}
		})
	}
	wait.Wait()
}
