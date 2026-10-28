package platform

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDomainCheckPathValidation(t *testing.T) {
	registry := NewRegistry()
	normalize := func(checkPath string) error {
		raw, _ := json.Marshal(map[string]any{
			"domain": "api.example.com", "check_path": checkPath,
		})
		_, err := registry.Normalize(KindDomainAdd, "env/api/production", raw)
		return err
	}
	for _, value := range []string{"", "/", "/users/sign-in", "/api/health"} {
		if err := normalize(value); err != nil {
			t.Errorf("check path %q refused: %v", value, err)
		}
	}
	for _, value := range []string{
		"users/sign-in", "https://api.example.com/users/sign-in", "//api.example.com/x",
		"/with space", "/with\nnewline", "/with\x00nul", "/with?query", "/with#fragment",
		"/with\x01control", "/with\x1b[31m", "/with\x7fdel", "/with\u009fcontrol",
		"/" + strings.Repeat("a", 256),
	} {
		if err := normalize(value); err == nil {
			t.Errorf("check path %q must be refused", value)
		}
	}
}

func TestDomainNormalizationKeepsTheCheckPath(t *testing.T) {
	registry := NewRegistry()
	raw, _ := json.Marshal(map[string]any{
		"domain": "api.example.com", "check_path": "/users/sign-in",
	})
	normalized, err := registry.Normalize(KindDomainAdd, "env/api/production", raw)
	if err != nil {
		t.Fatal(err)
	}
	input := normalized.Input.(*DomainInput)
	if input.CheckPath != "/users/sign-in" {
		t.Fatalf("check path = %q", input.CheckPath)
	}
}
