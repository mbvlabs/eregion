package platform

import (
	"encoding/json"
	"testing"
)

func TestEnvironmentBuildNodeIsValidatedAndDefaultsToRuntimeNode(t *testing.T) {
	registry := NewRegistry()
	normalize := func(input map[string]any) (Normalized, error) {
		raw, _ := json.Marshal(input)
		return registry.Normalize(KindEnvironmentCreate, "app/api", raw)
	}
	good, err := normalize(map[string]any{"name": "production", "node": "app-1", "build_node": "build-1"})
	if err != nil {
		t.Fatal(err)
	}
	input := good.Input.(*EnvironmentCreateInput)
	if input.BuildNode != "build-1" || input.Node != "app-1" {
		t.Fatalf("build placement was not preserved: %+v", input)
	}
	env := Environment{Node: "app-1", BuildNode: "build-1"}
	if env.BuildNodeName() != "build-1" {
		t.Fatalf("explicit build node was ignored: %s", env.BuildNodeName())
	}
	if (Environment{Node: "app-1"}).BuildNodeName() != "app-1" {
		t.Fatal("an omitted build node must default to the runtime node")
	}
	for name, input := range map[string]map[string]any{
		"invalid build node": {"name": "production", "node": "app-1", "build_node": "Build_1"},
		"empty node":         {"name": "production", "build_node": "build-1"},
	} {
		if _, err := normalize(input); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestEnvironmentPreviewMentionsDistinctBuildNode(t *testing.T) {
	input := &EnvironmentCreateInput{App: "api", Name: "production", Node: "app-1", BuildNode: "build-1"}
	if input.Target().String() != "app/api" {
		t.Fatalf("target=%s", input.Target())
	}
	// Preview is the input itself; the worker adds the registry-transfer warning.
	if got := input.Preview().(*EnvironmentCreateInput); got.BuildNode != "build-1" {
		t.Fatalf("preview lost build node: %+v", got)
	}
}
